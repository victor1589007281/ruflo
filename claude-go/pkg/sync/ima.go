package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// IMAAdapter 通过 node ima_api.cjs 调用 IMA OpenAPI。
type IMAAdapter struct {
	ClientID string
	APIKey   string
	Script   string
}

// NewIMAAdapter 创建 IMA 适配器。
//
// 脚本路径必须**现场探测**, 不能硬编码单一绝对路径 —— 硬编码正是 cron-1
// "IMA 笔记同步" 跑了 90 次挂 56 次的根因:
//
//   - 控制面跑在 k8s pod 里时, `/home/victor/.claude/skills/ima-skill` 这个 hostPath
//     是在 **kind 节点自己的文件系统**上解析的, 那里是个空目录; 宿主上的同名目录
//     其实有文件, 但两边看不见彼此。于是 node 报 MODULE_NOT_FOUND。
//   - pod 里真实存在的是 `/mnt/data/claude-go/skills/ima-skill` (state hostPath,
//     与宿主是同一份 ext4, 已实测 ima_api.cjs md5 一致)。
//
// 所以按候选列表逐个探测, 谁真实存在就用谁; 一个都不中就由 call() 报一条
// 把试过的路径全列出来的错。
func NewIMAAdapter(clientID, apiKey string) *IMAAdapter {
	a := &IMAAdapter{ClientID: clientID, APIKey: apiKey}
	a.Script = firstExistingFile(imaScriptCandidates())
	return a
}

// imaScriptCandidates 返回 ima_api.cjs 的候选路径, 按优先级排列。
func imaScriptCandidates() []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		for _, e := range out {
			if e == p {
				return
			}
		}
		out = append(out, p)
	}

	// ① 显式覆盖: 部署方要对冲这里的探测逻辑时, 用环境变量指死路径即可。
	add(strings.TrimSpace(os.Getenv("IMA_SCRIPT")))

	// ② 状态卷 (部署首选): state 目录在 pod 与宿主是同一份, 已实测存在。
	if d := strings.TrimSpace(os.Getenv("CLAUDE_GO_STATE_DIR")); d != "" {
		add(filepath.Join(d, "skills", "ima-skill", "ima_api.cjs"))
	}
	add("/mnt/data/claude-go/skills/ima-skill/ima_api.cjs")

	// ③ 宿主 skills 目录: 在交互机上直接跑控制面时它是对的。
	add("/home/victor/.claude/skills/ima-skill/ima_api.cjs")

	// ④ 相对 cwd 兜底 (源码树里直接跑)。
	if wd, err := os.Getwd(); err == nil {
		add(filepath.Join(wd, ".claude", "skills", "ima-skill", "ima_api.cjs"))
	}
	return out
}

// firstExistingFile 返回 candidates 中第一个真实存在的普通文件; 全不存在返回 ""。
func firstExistingFile(candidates []string) string {
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// nodeBinary 返回可用的 node 解释器; 找不到返回 ""。
// 显式覆盖(IMA_NODE_BIN)直接返回, 不在人间校验 —— 路径写错时由 exec 报出一条
// 带完整路径的错, 比在这里预检再报一句"文件不存在"更可操作。
func nodeBinary() string {
	if v := strings.TrimSpace(os.Getenv("IMA_NODE_BIN")); v != "" {
		return v
	}
	if p, err := exec.LookPath("node"); err == nil {
		return p
	}
	return ""
}

// Source 返回数据源名称。
func (a *IMAAdapter) Source() string { return "ima" }

// List 列出所有笔记摘要。
func (a *IMAAdapter) List() ([]ExternalItem, error) {
	body := map[string]interface{}{
		"folder_id": "",
		"sort_type": 1,
		"cursor":    "",
		"limit":     20,
	}
	optsJSON := a.optionsJSON()

	var all []ExternalItem
	cursor := ""
	for {
		body["cursor"] = cursor
		bodyJSON, _ := json.Marshal(body)
		out, err := a.call("openapi/note/v1/list_note", string(bodyJSON), optsJSON)
		if err != nil {
			return nil, err
		}

		var resp struct {
			Data struct {
				NoteBookList []struct {
					NoteID     string `json:"note_id"`
					Title      string `json:"title"`
					ModifyTime string `json:"modify_time"`
				} `json:"note_book_list"`
				Notes []struct {
					NoteID     string `json:"note_id"`
					Title      string `json:"title"`
					ModifyTime string `json:"modify_time"`
				} `json:"notes"`
				IsEnd  bool   `json:"is_end"`
				Cursor string `json:"cursor"`
			} `json:"data"`
		}
		if err := json.Unmarshal(out, &resp); err != nil {
			return nil, fmt.Errorf("解析笔记列表失败: %w", err)
		}

		list := resp.Data.NoteBookList
		if len(list) == 0 {
			list = resp.Data.Notes
		}

		for _, n := range list {
			updated := time.Now().UTC()
			if ms, err := strconv.ParseInt(n.ModifyTime, 10, 64); err == nil && ms > 0 {
				updated = time.Unix(ms/1000, 0).UTC()
			}
			all = append(all, ExternalItem{
				ExternalID: n.NoteID,
				Type:       "note",
				Title:      n.Title,
				URL:        "https://ima.qq.com/note/" + n.NoteID,
				UpdatedAt:  updated,
			})
		}

		if resp.Data.IsEnd || resp.Data.Cursor == "" {
			break
		}
		cursor = resp.Data.Cursor
	}
	return all, nil
}

// Fetch 拉取单条笔记正文。
func (a *IMAAdapter) Fetch(item ExternalItem) (ExternalItem, error) {
	body := map[string]interface{}{
		"note_id":               item.ExternalID,
		"target_content_format": 1,
	}
	bodyJSON, _ := json.Marshal(body)
	out, err := a.call("openapi/note/v1/get_doc_content", string(bodyJSON), a.optionsJSON())
	if err != nil {
		return item, err
	}

	var resp struct {
		Data struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return item, fmt.Errorf("解析笔记内容失败: %w", err)
	}
	if resp.Data.Title != "" {
		item.Title = resp.Data.Title
	}
	item.Body = resp.Data.Content
	if item.Body == "" {
		item.Body = "(空笔记)"
	}
	return item, nil
}

// optionsJSON 把 ClientID / APIKey 序列化成传给 ima_api.cjs 的 opts JSON。
// 空字段不写进 map, 免得用空串覆盖掉脚本侧的默认凭证来源。
func (a *IMAAdapter) optionsJSON() string {
	opts := map[string]string{}
	if a.ClientID != "" {
		opts["clientId"] = a.ClientID
	}
	if a.APIKey != "" {
		opts["apiKey"] = a.APIKey
	}
	data, _ := json.Marshal(opts)
	return string(data)
}

// call 起一个 node 子进程执行 ima_api.cjs 的指定接口, 返回其 stdout。
//
// 算法解读：
//
//	原理：IMA 只提供 node 版 SDK, 于是每次调用都 fork 一个 node 进程, 把 apiPath /
//	      body / opts 作为三个位置参数传进去, 从 stdout 拿 JSON。进程即隔离: 脚本崩了
//	      不会带走控制面, 也无需在 Go 侧复刻签名算法。
//	关键不变量：① 每页 2 分钟独立超时 (context.WithTimeout), 上层 List() 逐页调用,
//	      所以"每页"就是计时粒度 —— node 挂住时不会把一个 cron 任务拖成僵尸。
//	      ② 子进程退出码非 0 时把 stderr 原样拼进错误串, 否则只剩一句 "exit status 1"。
//	      ③ 业务错误不体现在退出码上: 必须再解一次 {code,msg}, code != 0 才算失败。
//	      ④ 无重试 —— 重试属于上层调度策略, 这里重试会把超时预算叠成 N 倍。
//	代价与边界：每页一次 fork, 笔记多时进程数线性增长; 每次调用都新建 ctx,
//	      没有跨页复用连接或会话。
//	设计权衡：脚本路径与 node 解释器都**不在这里校验**, 只在使用时由 exec 报错 ——
//	      报错信息里带上 script/node 全路径, 比预先 Stat 一句"文件不存在"更可操作。
func (a *IMAAdapter) call(apiPath, bodyJSON, optsJSON string) ([]byte, error) {
	if a.Script == "" {
		return nil, fmt.Errorf("找不到 ima_api.cjs: 已尝试 %s; "+
			"可用环境变量 IMA_SCRIPT 指死路径",
			strings.Join(imaScriptCandidates(), ", "))
	}
	node := nodeBinary()
	if node == "" {
		return nil, fmt.Errorf("PATH 上找不到 node 解释器 (ima_api.cjs 要求 node >= 18); " +
			"可用环境变量 IMA_NODE_BIN 指死路径")
	}

	// 子进程超时: node 挂住(网络直连 ima.qq.com 无代理时会发生)不该把一个
	// cron 任务拖成 15 分钟僵尸。上层 List() 逐页调用, 每页独立计时。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	args := []string{a.Script, apiPath, bodyJSON, optsJSON}
	cmd := exec.CommandContext(ctx, node, args...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		return nil, fmt.Errorf("调用 ima_api.cjs %s 失败 (script=%s node=%s): %w (stderr: %s)",
			apiPath, a.Script, node, err, stderr)
	}
	out = []byte(strings.TrimSpace(string(out)))
	// 检查业务错误码
	var base struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(out, &base)
	if base.Code != 0 {
		return nil, fmt.Errorf("IMA API %s 返回错误: code=%d msg=%s", apiPath, base.Code, base.Msg)
	}
	return out, nil
}

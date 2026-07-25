package dashboard

// run_feedback.go —— 奖励源 gate.e2e: 下游平台的门禁结果回传
// (design/03 §4.2 奖励源接线清单第 6 行 "e2e/下游验收")。
//
// ---------------------------------------------------------------------------
// 为什么这个端点必须落在 pkg/dashboard
// ---------------------------------------------------------------------------
//
// 设计原文写的就是 ":18080 新增 POST /api/runs/<id>/feedback (下游平台如 testforge 的
// 门禁结果回传) —— 兼容层新端点"。:18080 的 HTTP 面是飞书 bot 挂载的这份 dashboard
// mux (RegisterOn), :7777 独立 dashboard 用的是同一份, 所以一处注册两条入口都通。
//
// 这条源的价值在于它是**本进程之外**的确定性证据: 编译/测试门禁验的是"代码能不能
// 构建", e2e 验的是"部署出去以后跑不跑得起来" —— 后者 claude-go 自己永远算不出来,
// 只能由下游平台回传。所以它在权重表里与 gate.compile/gate.test 同为满权重 1.0
// (RewardSourceWeight 里 "gate.e2e" 早已登记, 本文件是把它真的接上)。
//
// ---------------------------------------------------------------------------
// 三处 fail-closed 的取舍 (都是"宁可不记, 不记假的")
// ---------------------------------------------------------------------------
//
//  1. **source 锁死为 gate.e2e**。不接受调用方自选源名 —— 允许自选等于把整张权重表
//     交给外部: 一个只能证明"e2e 跑过了"的调用方可以自称 gate.compile (确定性满权重)
//     甚至 user.explicit (人的判断), 那是 §4.6 防 reward hacking 里最直接的入口。
//     这与 §4.7 H7 的"锁定字段"同一条理由: 判据本身不在调用方的动作空间里。
//  2. **run 必须能在本机 teams/ 里认领**。认领不到直接 404 且不落盘。理由不是安全而是
//     数据有效性: AggregateRewards 按 (run_id, team) 过滤, 团队名对不上的奖励事件
//     落盘即死数据 —— 永远聚合不到, 只会把 rewards.jsonl 的尾部窗口挤满。
//  3. **既无 pass 也无 score 直接 400**。缺判据的"反馈"不是弱信号, 是没有信号;
//     给它补一个默认值就是凭空造奖励。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/agent"
)

// runFeedbackMaxBody 请求体上限。回传的是门禁结论不是日志正文, 64KiB 足够;
// detail 再长也会被 truncate 到 2KiB 才落盘。
const runFeedbackMaxBody = 64 << 10

// runFeedbackDetailMax detail 落盘上限 (rewards.jsonl 是学习器要倒序扫的流水,
// 一条几 MB 的门禁日志会把整个尾部窗口占满)。
const runFeedbackDetailMax = 2000

// runFeedbackReq 下游平台回传的门禁结论。
type runFeedbackReq struct {
	// Source 可选; 只允许缺省或字面量 "gate.e2e" (见文件头取舍 1)。
	Source string `json:"source,omitempty"`
	// Pass 二值门禁结论。Score 缺省时用它。
	Pass *bool `json:"pass,omitempty"`
	// Score 0-100 连续分 (如通过用例占比)。给了就优先用 —— 连续分信息量严格大于二值。
	Score *float64 `json:"score,omitempty"`
	// Suite 门禁标识 (如 "testforge:regression"), 只进 Raw 供人复盘。
	Suite string `json:"suite,omitempty"`
	// Detail 明细 (失败用例列表等), 只进 Raw。
	Detail string `json:"detail,omitempty"`
	// Node 归因到具体阶段; 留空则记 run 级。
	Node string `json:"node,omitempty"`
	// Team 可选的团队名断言: 给了就必须与本机认领到的一致, 否则 409。
	// 存在的理由是让调用方能发现自己拿错了 run id, 而不是把奖励静默记到别人头上。
	Team string `json:"team,omitempty"`
}

// runFeedbackResp 回执。
type runFeedbackResp struct {
	Recorded bool    `json:"recorded"`
	RunID    string  `json:"runId"`
	Team     string  `json:"team"`
	Node     string  `json:"node,omitempty"`
	Source   string  `json:"source"`
	Value    float64 `json:"value"`
	Weight   float64 `json:"weight"`
}

// handleRunFeedback 处理 POST /api/runs/{id}/feedback。
//
// 只接这一个子路径: /api/runs/ 前缀下的其它形状一律 404, 不做"顺手加个 GET 列表"——
// 那会让这个前缀变成第二个 team 只读面 (已有 /api/teams/), 两处各读一半必然漂移。
func (s *Server) handleRunFeedback(w http.ResponseWriter, r *http.Request) {
	runID, ok := parseRunFeedbackPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("仅支持 /api/runs/{runId}/feedback"))
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("POST only"))
		return
	}

	var req runFeedbackReq
	body, err := io.ReadAll(io.LimitReader(r.Body, runFeedbackMaxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("读取请求体失败: %w", err))
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("请求体不是合法 JSON: %w", err))
		return
	}
	if src := strings.TrimSpace(req.Source); src != "" && src != agent.RewardSourceGateE2E {
		writeError(w, http.StatusBadRequest, fmt.Errorf(
			"source 是锁定字段: 本端点只接受 %q, 收到 %q (design/03 §4.6 防 reward hacking)",
			agent.RewardSourceGateE2E, src))
		return
	}

	value, raw, err := runFeedbackValue(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	team, err := resolveRunTeam(s.cfg.StateDir, runID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if want := strings.TrimSpace(req.Team); want != "" && want != team {
		writeError(w, http.StatusConflict, fmt.Errorf(
			"run %s 属于团队 %q, 与请求声明的 %q 不一致 —— 拒绝落盘 (可能拿错了 run id)",
			runID, team, want))
		return
	}

	ee := feedbackEngine(s.cfg.StateDir)
	if ee == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("进化引擎数据目录不可用, 奖励无处记录"))
		return
	}
	ee.RecordReward(agent.RewardEvent{
		RunID:  runID,
		NodeID: strings.TrimSpace(req.Node),
		Source: agent.RewardSourceGateE2E,
		Value:  value,
		Raw:    raw,
		Team:   team,
	})
	writeJSON(w, http.StatusAccepted, runFeedbackResp{
		Recorded: true,
		RunID:    runID,
		Team:     team,
		Node:     strings.TrimSpace(req.Node),
		Source:   agent.RewardSourceGateE2E,
		Value:    value,
		Weight:   agent.RewardSourceWeight(agent.RewardSourceGateE2E),
	})
}

// parseRunFeedbackPath 从 /api/runs/{id}/feedback 取 run id。形状不对返回 false。
func parseRunFeedbackPath(path string) (string, bool) {
	rest := strings.TrimPrefix(path, "/api/runs/")
	if rest == path {
		return "", false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[1] != "feedback" {
		return "", false
	}
	id := strings.TrimSpace(parts[0])
	// run id 会被当成 rewards.jsonl 的过滤键, 不进文件路径; 但仍拒绝路径穿越形状的值,
	// 免得将来有人拿它拼路径 (轨迹桶名就是按 trace id 拼的)。
	if id == "" || strings.Contains(id, "..") {
		return "", false
	}
	return id, true
}

// runFeedbackValue 把回传结论归一化到 [-1,1], 并造 Raw 原始值。
//
// score 优先于 pass: 连续分的信息量严格大于二值 (80 分和 51 分对学习是两回事)。
// 映射 score/50-1 与 gate.content / review.panel 完全一致 —— 同一套 0-100 分口径
// 在本仓只该有一个映射, 否则跨源比较就没有意义了。
func runFeedbackValue(req runFeedbackReq) (float64, map[string]any, error) {
	raw := map[string]any{}
	if s := strings.TrimSpace(req.Suite); s != "" {
		raw["suite"] = s
	}
	if d := strings.TrimSpace(req.Detail); d != "" {
		if len(d) > runFeedbackDetailMax {
			d = d[:runFeedbackDetailMax] + "…[truncated]"
		}
		raw["detail"] = d
	}
	switch {
	case req.Score != nil:
		sc := *req.Score
		if sc < 0 || sc > 100 {
			return 0, nil, fmt.Errorf("score 须在 0-100, 收到 %v", sc)
		}
		raw["score"] = sc
		if req.Pass != nil {
			raw["pass"] = *req.Pass
		}
		return sc/50.0 - 1.0, raw, nil
	case req.Pass != nil:
		raw["pass"] = *req.Pass
		if *req.Pass {
			return 1, raw, nil
		}
		return -1, raw, nil
	default:
		return 0, nil, errors.New("必须给出 pass (二值) 或 score (0-100) 之一 —— 无判据的反馈不是弱信号而是没有信号")
	}
}

// resolveRunTeam 从 teams/<name>/team.json 的 lastRunId 认领这个 run 属于哪个团队。
//
// 为什么不信调用方传的 team: 奖励的归因键是 (run_id, team), 两者对不上就是死数据。
// 由本机的持久化状态来认领是唯一可靠的口径 —— 而且顺带把"随手 POST 一个不存在的
// run id"挡在门外。
//
// 只认 lastRunId 是已知局限: 一个团队跑过 N 轮, 只有最近一轮认得出来。历史轮次的
// e2e 回传会 404。修它需要 run→team 的独立索引, 那是另一件事; 现状下 e2e 回传本来
// 就发生在交付后不久, 覆盖最近一轮已经够用。记账在此, 不假装做完了。
func resolveRunTeam(stateDir, runID string) (string, error) {
	root := filepath.Join(stateDir, "teams")
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("读取 teams 目录失败: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, e.Name(), "team.json"))
		if err != nil {
			continue
		}
		var t struct {
			Name      string `json:"name"`
			LastRunID string `json:"lastRunId"`
		}
		if json.Unmarshal(data, &t) != nil {
			continue
		}
		if t.LastRunID != "" && t.LastRunID == runID {
			if strings.TrimSpace(t.Name) != "" {
				return t.Name, nil
			}
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("本机没有 run %q 的归属团队 (只认各团队最近一轮的 lastRunId), 拒绝记录无归因的奖励", runID)
}

// ---------------------------------------------------------------------------
// EvolutionEngine 复用
// ---------------------------------------------------------------------------

// 按 dataDir 缓存 EvolutionEngine。
//
// 为什么缓存: NewEvolutionEngine 会 load 整份 experiences.json 并做一次字段迁移,
// 每个请求造一个等于把一次外部 POST 变成一次全量读盘。
//
// 为什么放包级变量而不是 Server 字段: 这条端点是往一个共享进程 (dashboard 同时被
// :7777 独立进程与 :18080 飞书进程装配) 里加的一件事, 改 Server 结构体会牵动所有
// 装配处。缓存键是 dataDir, 同一进程内多个 Server 实例指向同一目录时共用一份 ——
// 那正是想要的 (RecordReward 的互斥锁才有意义)。
var (
	feedbackEvoMu sync.Mutex
	feedbackEvos  = map[string]*agent.EvolutionEngine{}
)

// feedbackEngine 取(或建)该状态目录的进化引擎。stateDir 为空时返回 nil。
func feedbackEngine(stateDir string) *agent.EvolutionEngine {
	if strings.TrimSpace(stateDir) == "" {
		return nil
	}
	dir := filepath.Join(stateDir, "evolution")
	feedbackEvoMu.Lock()
	defer feedbackEvoMu.Unlock()
	if ee, ok := feedbackEvos[dir]; ok {
		return ee
	}
	// llm 传 nil: 本端点只写 rewards.jsonl, 不做任何蒸馏 —— 给它一个 LLM 客户端只会
	// 让一条外部 POST 有能力触发 LLM 花费。
	ee := agent.NewEvolutionEngine(dir, nil)
	feedbackEvos[dir] = ee
	return ee
}

package feishu

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/basedir"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/sessionapi"
)

// session_spec.go — 会话身份的声明与落盘 ("会话即员工" 的那一半)。
//
// # 为什么落盘
//
// SessionManager.sessions 是纯内存 map (session.go:124): 进程一重启, 会话的
// 人格/工具白名单/工作目录/模型就全没了, 而**对话历史还在** (transcript 快照)。
// 那个组合是最坏的一种: 一个恢复了全部上下文的会话, 却失去它的全部约束 ——
// 于是它下一轮开始用一个"更宽松的自己"继续干上次的活。
//
// spec 落盘到 <state>/sessions/<id>.json, 引擎在**第一条消息到达时**按 spec 构造
// (惰性), 历史经 transcript 快照读回。这条链与 `claude-go run --resume` 同源。
//
// # 为什么 id 要过一道白名单
//
// id 直接参与文件路径 (<sessions>/<id>.json)。若放任任意字符串, 一个
// `../../teams/foo` 就能让"建会话"写穿到别的目录 —— 而 HTTP 面上这个 id 来自
// 请求体。所以这里**拒绝**而不是转义: 转义会造出一个与原 id 不同的文件名,
// 于是 List 时的 id 与盘上的文件名对不上, 变成另一种静默错乱。

// sessionIDPattern 合法会话 id: 字母数字与 `_ . -`, 2~64 字符。
//
// 用锚定的全匹配而不是"禁止 /": 后者要穷举所有危险字符 (还有 `..`、空字节、
// Windows 保留名…), 而白名单只需说清什么是对的。
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidSessionID 报告 id 是否可用作会话标识 (= 文件名)。
//
// 额外的两条 (正则表达不了):
//   - 不能以 `.` 开头 (否则 `.` / `..` 这类会被正则放过的名字能逃出去)
//   - 不能含路径分隔符 (正则已挡, 这里显式再挡一次: 它是安全边界, 不该只靠一条
//     正则记得住)
func ValidSessionID(id string) bool {
	if !sessionIDPattern.MatchString(id) {
		return false
	}
	if strings.HasPrefix(id, ".") {
		return false
	}
	if strings.ContainsAny(id, `/\`) {
		return false
	}
	return true
}

// specStore 是 spec 的落盘存储 (一个目录)。
type specStore struct {
	dir string

	mu    sync.RWMutex
	cache map[string]sessionapi.Spec // id → spec (已知的全集)
	once  sync.Once
}

// newSpecStore 按 StateDir/Cwd 推导 <state>/sessions。
//
// 用 basedir 而不是自己拼字符串: 目录布局有一处声明 (basedir.NewLayout),
// 各处硬拼会让"加一个子目录"变成一次跨文件搜索。
func newSpecStore(stateDir, cwd string) *specStore {
	layout, err := basedir.NewLayout(basedir.ResolveDefault(stateDir, cwd))
	if err != nil {
		// 没有可用的 state 根 = 无法落盘。返回一个 dir 为空的 store, 它的所有
		// 存取都是 no-op —— 与 "未注入 StateStore 时历史快照关闭" 同一处理:
		// 增强项缺失不该让会话本身不可用。
		return &specStore{cache: map[string]sessionapi.Spec{}}
	}
	return &specStore{dir: layout.Sessions, cache: map[string]sessionapi.Spec{}}
}

// load 一次性把盘上所有 spec 读进缓存。
//
// 全量载入而不是按需读: 会话数量是"员工数"量级 (几十), 而 List 要返回全集 ——
// 按需读会让 List 变成 N 次 IO。读失败的文件逐个跳过并记账, 不整体失败:
// 一个坏文件不该让其余几十个员工全部消失。
func (s *specStore) load() {
	s.once.Do(func() {
		if s.dir == "" {
			return
		}
		entries, err := os.ReadDir(s.dir)
		if err != nil {
			return // 目录还没建 (首次启动) 是正常路径, 不报错
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
			if err != nil {
				continue
			}
			var sp sessionapi.Spec
			if err := json.Unmarshal(raw, &sp); err != nil {
				continue
			}
			if sp.ID == "" {
				continue
			}
			s.cache[sp.ID] = sp
		}
	})
}

func (s *specStore) put(sp sessionapi.Spec) error {
	s.mu.Lock()
	s.cache[sp.ID] = sp
	s.mu.Unlock()
	if s.dir == "" {
		return nil
	}
	raw, err := json.MarshalIndent(sp, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	// 先写临时文件再 rename: 与 statestore 的 KV 落盘同一手法 —— 直接覆写时
	// 一次中途失败会留下半截 JSON, 而那读回来只会被当成"这个员工没配过"。
	tmp := filepath.Join(s.dir, sp.ID+".json.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, sp.ID+".json"))
}

func (s *specStore) delete(id string) error {
	s.mu.Lock()
	delete(s.cache, id)
	s.mu.Unlock()
	if s.dir == "" {
		return nil
	}
	if err := os.Remove(filepath.Join(s.dir, id+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *specStore) get(id string) (sessionapi.Spec, bool) {
	s.load()
	s.mu.RLock()
	defer s.mu.RUnlock()
	sp, ok := s.cache[id]
	return sp, ok
}

func (s *specStore) all() []sessionapi.Spec {
	s.load()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]sessionapi.Spec, 0, len(s.cache))
	for _, sp := range s.cache {
		out = append(out, sp)
	}
	return out
}

// applySpecTo 把白名单声明应用到引擎配置上。
//
// ★ 调用点在 createSessionWithClient **建引擎之前** —— AllowedTools 参与引擎构造
// (ToolGateHook 在 NewQueryEngine 里按它注册, engine.go:737), 构造之后再改就只是
// 改了一个没人读的结构体。
//
// cwd 与 systemPrompt 不在这里: 它们各自的落点在 createSessionWithClient 里,
// 且 cwd 要同时喂给 prompt.NewManager 与 engine.Config 两处 (见那儿的 effCwd)。
func applySpecTo(cfg *engine.Config, sp sessionapi.Spec) {
	if m, ok := sp.AllowedToolsMap(); ok {
		// 空 map 即全拒 —— 这是刻意的 fail-closed, 不是 "没配"。
		cfg.AllowedTools = m
	}
	if sp.Cwd != "" {
		cfg.Cwd = expandPath(sp.Cwd)
	}
	if sp.MaxRunTokens > 0 {
		// run 级 token 止损 (考场类会话的双闸门之一, 另一道是平台的倒计时)。
		cfg.MaxRunTokens = sp.MaxRunTokens
	}
}

// expandPath 把 ~ 前缀展开成 home。
//
// 为什么要管这个: Cwd 来自 HTTP 请求体, 而 `~` 在 shell 里由 shell 展开 ——
// 写 "~/work" 的人期待的是家目录, 但一个不经 shell 的进程会把它当成字面量,
// 得到一个叫 "~" 的不存在目录。工具随后在"不存在的目录"里跑, 报的错与权限、
// 与配置都无关, 极难归因。空串原样返回 (由调用方决定回落)。
func expandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// allowedToolsSummary 给日志用的一句话描述。
//
// 三态必须**分别**打印: 把"全拒"印成空字符串, 与"不限"在日志里长得一模一样 ——
// 而这两个的后果差着一个数量级。这与本仓 "成功路径也必须报数" 那条同向:
// 只在异常时报数, 正常与没跑就分不开。
func allowedToolsSummary(sp sessionapi.Spec) string {
	if sp.AllowedTools == nil {
		return "不限"
	}
	if len(*sp.AllowedTools) == 0 {
		return "全拒"
	}
	return strings.Join(*sp.AllowedTools, ",")
}

// specString 给日志一行可读的身份。
func specString(sp sessionapi.Spec) string {
	return fmt.Sprintf("session=%s name=%q model=%q cwd=%q tools=[%s]",
		sp.ID, sp.Name, sp.ModelAlias, sp.Cwd, allowedToolsSummary(sp))
}

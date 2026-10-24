// Package sessionapi 是**活会话**的 HTTP 面: 建会话、发消息、读回复、列举、清除。
//
// # 它与 pkg/session 的分工 (别混)
//
//	pkg/session     —— transcript 的落盘格式与读写 (磁盘上的会话历史)
//	pkg/sessionapi  —— 一个**正在跑的**会话的对外接口 (内存里的引擎 + 它的历史)
//
// 两者通过 Host 接口交界: Host 的实现方 (pkg/feishu 的 SessionManager) 自己决定
// 历史存哪、引擎怎么建, 本包一概不管。
//
// # 为什么是异步 + 轮询, 不是同步阻塞
//
// 平台侧一次真实巡检实测 223 秒。同步接口会撞 HTTP 超时, 而超时之后调用方**分不清**
// "它还在跑" 与 "它挂了" —— 两者都是连接断开。异步 + 轮询把这两件事分开:
//
//	POST .../messages      → 202 {runId, status:"running"}
//	GET  .../runs/{runId}  → {"status":"completed", "reply":"...", "tokens":{...}}
//
// # 为什么忙的时候返回 409 而不是排队
//
// pkg/feishu 的 Session 有 EnqueuePending (最多缓存 1 条, 新的覆盖旧的), 但它的
// 回复是个回调 —— 对轮询调用方没有可用语义, 而且"排队了"这个事实调用方看不见。
// 409 让"现在不行"变成一条明确的、可重试的信号。这是刻意的选择, 不是没实现。
package sessionapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
)

// Spec 一个会话的身份声明。
//
// "会话即员工": 名字、人格、工具白名单、工作目录、模型都在这里定。**不是**全局配置 ——
// 这正是一个会话与另一个会话之间的区别所在。
type Spec struct {
	ID           string `json:"id"`
	Name         string `json:"name,omitempty"`         // 展示名 (轨迹分组用); 空则回落成 ID
	SystemPrompt string `json:"systemPrompt,omitempty"` // 人格
	Cwd          string `json:"cwd,omitempty"`
	ModelAlias   string `json:"modelAlias,omitempty"`

	// AllowedTools 工具白名单。**三态**, 一个都不能少:
	//
	//	nil (字段缺席)       → 不限 (沿用引擎默认工具集)
	//	&[]string{} (空数组) → **全部拒绝** (fail-closed)
	//	&[]string{"Bash"}    → 只允许 Bash
	//
	// ★ 为什么是指针而不是 []string: 用值类型 + `omitempty` 序列化时, **空数组会被
	// 整个丢掉** —— 于是一次落盘-读回的往返会把"全拒"悄悄变成"不限"。那是这个字段
	// 最危险的一种退化: 声明越严, 重启后越松, 而且没有任何提示。指针的 omitempty
	// 只在 nil 时省略, 非 nil 的空切片会如实写成 `[]`。
	//
	// 引擎侧的对应语义见 engine.Config.AllowedTools (engine.go:237-243、:335)。
	AllowedTools *[]string `json:"allowedTools,omitempty"`

	// MaxRunTokens 单轮 (一次 messages 调用) 的 token 消耗上限, 0 = 不设限。
	// 装配进 engine.Config.MaxRunTokens (applySpecTo)。考场/批处理类会话用它做
	// run 级止损, 防止一条消息触发无止境的 agentic 循环把额度烧穿。
	MaxRunTokens int `json:"maxRunTokens,omitempty"`

	// ToolProfile 钉死工具档位 (chat|research|coding|team|analysis|admin)。
	//
	// 缺省为空 = 维持原行为: 每条消息按文本启发式现猜 (inferFeishuToolProfile)。
	// 猜的坏处对**机器调用方**是致命的: "用 Bash 跑 echo" 里一个中文关键词都没有,
	// 就被分到 chat 档 —— 然后 AllowedTools 白名单从空工具集里收窄, 模型面对一个
	// 什么都没有的工具箱 (装配自报只在日志里, HTTP 调用方永远看不到)。
	// 考场/巡检这类由平台驱动的会话必须钉档, 不允许按消息内容漂移。
	ToolProfile string `json:"toolProfile,omitempty"`
}

// ToolsAllow 是构造"只允许这些工具"的白名单的便捷写法。
func ToolsAllow(names ...string) *[]string { return &names }

// ToolsDenyAll 是构造"全部拒绝"的便捷写法 (与字段缺席的"不限"是一对)。
func ToolsDenyAll() *[]string { return &[]string{} }

// AllowedToolsMap 把三态白名单编译成 engine.Config.AllowedTools 的形态。
//
//	Spec.AllowedTools == nil  → 返回 (nil, false)  不限
//	否则                       → 返回 (map, true)  按 map 判, 空 map 即全拒
//
// 第二个返回值用来区分"不限"与"空 map" —— 引擎两者都表达得了, 但调用方需要知道
// 该不该设那个字段 (设一个 nil 与不设是同一件事, 设一个空 map 是"全拒")。
//
// 导出而不是留在 feishu 侧: "三态怎么编译成引擎的二态" 是 Spec 自己的语义,
// 每个适配器各写一遍迟早会分叉出三种不同的解释。
func (s Spec) AllowedToolsMap() (map[string]bool, bool) {
	if s.AllowedTools == nil {
		return nil, false
	}
	m := make(map[string]bool, len(*s.AllowedTools))
	for _, n := range *s.AllowedTools {
		m[n] = true
	}
	return m, true
}

// DisplayName 会话的展示名: Name 为空时回落成 ID。
// 回落值是**有意义**的 (它就是会话的身份), 不是占位符。
func (s Spec) DisplayName() string {
	if strings.TrimSpace(s.Name) != "" {
		return s.Name
	}
	return s.ID
}

// Host 是会话的实际持有方 (生产上是 pkg/feishu.SessionManager)。
//
// 只放这 6 个方法: 本包不需要知道引擎怎么建、历史存哪。接口窄是刻意的 ——
// 它让"把 HTTP 面接上"这件事对实现方只有一个适配器的成本。
type Host interface {
	// Ensure 建立或更新一个会话的 Spec。幂等: 同 ID 再调 = 覆盖 Spec。
	Ensure(spec Spec) error
	// List 列出所有已知会话的 Spec。
	List() []Spec
	// Get 取单个会话的 Spec; 不存在返回 ErrNotFound。
	Get(id string) (Spec, error)
	// Delete 删除会话的 Spec (不动历史文件 —— 那是 Clear 的事)。
	Delete(id string) error
	// Clear 清空会话的对话历史 (Spec 保留)。
	Clear(id string) error
	// Busy 该会话是否正在跑 (或已被占住)。**只读探测**, 不做占位。
	Busy(id string) bool
	// Reserve 为一次即将开始的运行**占住**会话并分配这次运行的 runID; 已占用时
	// 返回 ErrBusy。
	//
	// ★ 它在 postMessage 里是**同步**调用、且发生在返回 202 之前 —— 那是 409 的
	// 权威判据。若改用 Busy 探测, 从"探测为闲"到"Run 真的把 processing 置起来"
	// 之间有一个窗口, 两条同时到达的消息会双双通过检查、双双拿到 202, 然后其中
	// 一条在会话内部被静默丢弃 —— 而调用方手里握着两个 runId, 等一个永远不会完成的
	// 运行。占位把"现在不行"变成一个**原子**事实。
	//
	// ★ runID 由**实现方**分配而不是本包自己编: 它同时是轨迹桶的 TraceID
	// (见 pkg/feishu/session_trace.go), 由实现方在起跑前写进 ctx。本包另编一个
	// 只会得到两个互不相干的 id —— 轮询拿到的 runId 读不出任何 span, 而
	// "读不到"与"没采集"在读数上一模一样。
	//
	// 占位由 Run 在退出时释放 (实现方负责, 含 panic 路径); 调用方 Reserve 成功后
	// **必须**调 Run。
	Reserve(id string) (runID string, err error)
	// Run 在会话里跑一条消息并返回最终回复。**本包在 goroutine 里调它** ——
	// 实现方可以是阻塞的, 但必须遵守 ctx 取消。
	Run(ctx context.Context, id, text string) (string, error)
	// Tokens 取某次运行消耗的 token。实现方采不到时返回 (nil, nil) ——
	// 那是"采不到", 与"消耗为 0"是两件事, 必须可区分。
	//
	// 拿不到轨迹底座的实现方可以返回 (nil, nil) 恒真。
	Tokens(runID string) (*Usage, error)
}

// StreamHost 可选能力: Host 若能流式暴露一次运行的中间事件 (文本增量/工具调用),
// 实现它, postMessage 就会改走 RunStream 并把事件记入缓冲, 供
// GET .../runs/{runId}/events 回放与订阅。
//
// 可选而不是加进 Host: Host 只有 6 个方法的窄接口是刻意的 (见 Host 注释),
// 流式是"有就更好"的增强, 不该把没有事件源的实现方挡在门外。
type StreamHost interface {
	RunStream(ctx context.Context, id, text string, emit func(Event)) (string, error)
}

// InjectHost 可选能力: 往**正在跑的** run 里注入一条 user 消息 (运行中插话)。
// 返回 false 表示"现在没人接" (会话空闲) —— 调用方据此改走普通 messages。
type InjectHost interface {
	Inject(id, text string) bool
}

// TraceHost 可选能力: 导出一个 run 的结构化轨迹 (prompt/工具调用/结果全文),
// 供外部评价器使用。数据来源是轨迹底座 (pkg/evolution/tracestore), runID 即
// postMessage 返回的轮询键 —— 两处逐字节相同是 Reserve 的刻意设计。
type TraceHost interface {
	Trace(runID string) ([]tracestore.Span, error)
}

// Usage 一次运行的 token 消耗。
//
// ★ 用**指针**返回、字段带 omitempty: "采不到" (整个 Usage 为 nil) 与"确实消耗 0"
// 在轮询侧必须能分开。恒 0 与没采集长得一样的坑, 本仓已经踩过一次
// (见 shared-pointer / 任务耗时恒 0 那类缺陷)。
type Usage struct {
	In          int `json:"in"`
	Out         int `json:"out"`
	CacheRead   int `json:"cacheRead,omitempty"`
	CacheCreate int `json:"cacheCreation,omitempty"`
	Calls       int `json:"calls"`
}

// ErrNotFound 会话不存在。
var ErrNotFound = errors.New("session not found")

// ErrBusy 会话已被占住 (另一次运行正在进行)。→ 409。
//
// 与 ErrNotFound 分开是两个不同的动作: 前者要**重试**, 后者要**先建会话**。
// 合成一个"出错了"会让调用方在两件该做不同事的情况之间瞎猜。
var ErrBusy = errors.New("session busy")

// RunStatus 一次运行的状态机。
type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	// RunCancelled 调用方主动取消 (POST .../runs/{runId}/cancel)。与 failed 分开:
	// 取消是**正常**的运维动作, 混进 failed 会让"为什么挂了"的排查每次都先
	// 排掉一遍"其实只是被取消了"。
	RunCancelled RunStatus = "cancelled"
)

// Run 一次运行的档案。
type Run struct {
	ID        string    `json:"runId"`
	SessionID string    `json:"sessionId"`
	Status    RunStatus `json:"status"`
	Reply     string    `json:"reply,omitempty"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt,omitempty"`

	// events 本次运行的事件缓冲 (SSE 出口的料, 见 events.go)。不序列化 ——
	// 它走自己的端点, 混进 runView 会让每次轮询都把全量事件背一遍。
	events *eventLog
	// cancel 取消本次运行 (G3)。只在本 run 存活期有效, 终态后置 nil。
	cancel context.CancelFunc
}

// maxRetainedRuns 每个会话最多留几份运行档案。
//
// 为什么要封顶: 这是一个**常驻**进程, run 表只增不减的话, 一个跑几个月的控制面
// 会把它吃光。淘汰按 StartedAt 从旧到新。
const maxRetainedRuns = 200

// Server 会话 HTTP 面的处理器。
type Server struct {
	host func() Host

	mu   sync.RWMutex
	runs map[string][]*Run // sessionID → 运行档案 (新→旧)
}

// New 构造。host 是**惰性取**的: 生产上这个闭包在 serve 装配期就注册, 而那时
// Host 的持有者 (feishu.Bot) 还没构造出来 (见 cmd/claude-go/main.go 的 APIExtensions 段)。
// 传一个已构造好的值会让那段装配直接崩。
func New(host func() Host) *Server {
	return &Server{host: host, runs: map[string][]*Run{}}
}

// Mount 把路由挂到给定的 mux 上。
//
// ★ 刻意**不**自己起 listener: 挂到 wiki API 的同一个 mux (pkg/wiki/api.go:94 的
// Mux()), 鉴权就自动被 httpauth.Middleware 覆盖 (api.go:126)。自己起一个端口
// 会绕开那道中间件 —— 本仓在 dashboard 与 cluster 路由上踩过完全同型的坑
// (api.go 的 Start 注释里记着: "历史上鉴权是逐路由包装的, 结果这两组都漏了")。
func Mount(mux *http.ServeMux, host func() Host) *Server {
	s := New(host)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/sessions/", s.handleSessionItem)
	// 轨迹导出 (G2): 按 runID 直查, 不挂在会话路径下 —— 评价器手里只有 runId
	// (postMessage 的返回值), 不该要求它再记一遍会话 id 才能取轨迹。
	mux.HandleFunc("/api/traces/", s.handleTrace)
	return s
}

// ── 路由 ────────────────────────────────────────────────────────────────────

// handleSessions GET/POST /api/sessions: 列出会话 (带 busy 标记, 按 id 排序) 或创建会话。
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	h := s.host()
	if h == nil {
		writeErr(w, http.StatusServiceUnavailable, "会话运行时尚未就绪")
		return
	}
	switch r.Method {
	case http.MethodGet:
		specs := h.List()
		sort.Slice(specs, func(i, j int) bool { return specs[i].ID < specs[j].ID })
		out := make([]map[string]any, 0, len(specs))
		for _, sp := range specs {
			out = append(out, map[string]any{"spec": sp, "busy": h.Busy(sp.ID)})
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
	case http.MethodPost:
		var sp Spec
		if err := json.NewDecoder(r.Body).Decode(&sp); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
			return
		}
		if strings.TrimSpace(sp.ID) == "" {
			writeErr(w, http.StatusBadRequest, "id 不能为空")
			return
		}
		if err := h.Ensure(sp); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, sp)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "只支持 GET / POST")
	}
}

// handleSessionItem 分派 /api/sessions/{id}[/{sub}...]。
//
// 手写前缀切分而不是用 Go 1.22+ 的 pattern 路由 ("/api/sessions/{id}"):
// 本项目其余路由 (dashboard、cluster、wiki) 全是 HandleFunc + 前缀判断, 保持一致;
// 混用两套风格会让"这条路由的匹配规则是什么"在文件之间来回跳。
func (s *Server) handleSessionItem(w http.ResponseWriter, r *http.Request) {
	h := s.host()
	if h == nil {
		writeErr(w, http.StatusServiceUnavailable, "会话运行时尚未就绪")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	if rest == "" {
		writeErr(w, http.StatusNotFound, "缺少会话 id")
		return
	}
	parts := strings.Split(rest, "/")
	id := parts[0]
	sub := parts[1:]

	// 会话级端点: /api/sessions/{id}
	if len(sub) == 0 {
		switch r.Method {
		case http.MethodGet:
			sp, err := h.Get(id)
			if err != nil {
				s.writeHostErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"spec":        sp,
				"busy":        h.Busy(id),
				"lastRun":     s.lastRun(id),
				"recentRuns":  s.recentRunIDs(id, 10),
				"runningRuns": s.runningRunIDs(id),
			})
		case http.MethodDelete:
			if err := h.Delete(id); err != nil {
				s.writeHostErr(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "只支持 GET / DELETE")
		}
		return
	}

	switch sub[0] {
	case "clear":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		if err := h.Clear(id); err != nil {
			s.writeHostErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case "messages":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		s.postMessage(w, r, h, id)

	case "inject":
		// 运行中插话 (G3): 往正在跑的 run 塞一条 user 消息, 轮次边界被取走。
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "只支持 POST")
			return
		}
		s.postInject(w, r, h, id)

	case "runs":
		if len(sub) < 2 || sub[1] == "" {
			if r.Method != http.MethodGet {
				writeErr(w, http.StatusMethodNotAllowed, "只支持 GET")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"runs": s.runsFor(id)})
			return
		}
		if len(sub) >= 3 {
			switch sub[2] {
			case "events":
				if r.Method != http.MethodGet {
					writeErr(w, http.StatusMethodNotAllowed, "只支持 GET")
					return
				}
				s.handleRunEvents(w, r, id, sub[1])
				return
			case "cancel":
				if r.Method != http.MethodPost {
					writeErr(w, http.StatusMethodNotAllowed, "只支持 POST")
					return
				}
				s.postCancel(w, id, sub[1])
				return
			}
		}
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "只支持 GET")
			return
		}
		run := s.findRun(id, sub[1])
		if run == nil {
			writeErr(w, http.StatusNotFound, "没有这次运行: "+sub[1])
			return
		}
		writeJSON(w, http.StatusOK, s.runView(h, run))

	default:
		writeErr(w, http.StatusNotFound, "未知子路径: "+sub[0])
	}
}

// postMessage POST /api/sessions/{id}/messages: 原子占位后异步跑一轮, 立即 202 返回 runId。
//
// 算法解读：
//
//	原理：一次消息 = 一次 run。HTTP 层只负责"占位 + 起 goroutine + 立刻 202"; 真正的执行
//	      结果通过 run 档案表 (轮询) 与 SSE 事件流 (订阅) 两条路回给调用方。
//	关键不变量：占位 (Reserve) 必须原子 —— 否则两条同时到达的消息会双双通过、双双拿到 202,
//	      同一个会话上跑起两个 run。收口必须写在 defer 里: 任何一条退出路径 (含 panic) 都要
//	      落到终态并释放占位, 否则这个会话永久 409。
//	代价与边界：同一会话同时只能有一个 run, 忙时返回 409 而不是排队; 取消的权威判据是本 run
//	      的 ctx 而不是 Run 返回的 err —— 引擎被取消时可能只是关掉事件流并正常返回。
//	设计权衡：异步 + runId 而不是同步长连接, 让慢 run 不占用调用方的连接预算; 代价是调用方
//	      要多一次轮询或订阅。
func (s *Server) postMessage(w http.ResponseWriter, r *http.Request, h Host, id string) {
	if _, err := h.Get(id); err != nil {
		s.writeHostErr(w, err)
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeErr(w, http.StatusBadRequest, "text 不能为空")
		return
	}
	// ★ 占位 (Reserve) 而不是探测 (Busy): 这条检查必须是**原子**的,
	// 否则两条同时到达的消息会双双通过、双双拿到 202 (见 Host.Reserve 的注释)。
	runID, err := h.Reserve(id)
	if err != nil {
		if errors.Is(err, ErrBusy) {
			// 409 而不是排队 —— 见包注释。带上正在跑的那个 runId, 让调用方知道**等谁**,
			// 否则它只知道"忙", 只能盲重试。
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":        "该会话正在处理上一条消息",
				"busy":         true,
				"runningRunId": firstOrEmpty(s.runningRunIDs(id)),
			})
			return
		}
		s.writeHostErr(w, err)
		return
	}

	run := &Run{
		ID:        runID,
		SessionID: id,
		Status:    RunRunning,
		StartedAt: time.Now(),
		events:    newEventLog(),
	}
	s.remember(run)

	// 可取消的 run ctx (G3): POST .../runs/{runId}/cancel 触发。原来是
	// context.Background() —— HTTP 层没有任何手段让一个跑飞的 run 停下来,
	// 只能干等它自己结束 (或把会话删了, 连同历史一起)。
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel

	go func() {
		// panic 也要落到终态。留着 run 停在 running 的后果是: 轮询方**永远**等下去,
		// 而这个会话因为占位没释放, 后续消息全部 409 —— 一次 panic 让这个会话永久死亡。
		// 收口写在 defer 里, 与 pkg/feishu 的 endRun 同一理由 (那是唯一"无论从哪条路径
		// 退出都会走一遍"的位置)。
		var reply string
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("会话运行 panic: %v", r)
				}
			}()
			// 有流式能力的 host 走 RunStream: 中间事件 (文本增量/工具调用) 进
			// 本 run 的事件缓冲, SSE 端点 (events.go) 随即推给订阅方。
			// 没有则退回阻塞式 Run —— 能力降级, 不是错误。
			if sh, ok := h.(StreamHost); ok {
				reply, err = sh.RunStream(ctx, id, body.Text, func(ev Event) {
					run.events.append(ev)
				})
			} else {
				reply, err = h.Run(ctx, id, body.Text)
			}
		}()

		s.mu.Lock()
		run.EndedAt = time.Now()
		run.cancel = nil
		switch {
		// 取消的权威判据是**本 run 的 ctx**, 不是 Run 返回的 err: 引擎在被取消时
		// 可能只是关掉事件流、正常返回 (queryLoop 视 ctx.Done 为干净退出),
		// 只看 err 会把一次取消记成 completed —— "停了但没留痕"。
		case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
			run.Status = RunCancelled
			run.Error = "调用方取消"
			if reply != "" {
				run.Reply = reply // 取消前已产出的半截回复保留, 便于复盘
			}
		case err != nil:
			run.Status = RunFailed
			run.Error = err.Error()
		default:
			run.Status = RunCompleted
			run.Reply = reply
		}
		final := run.Status
		errMsg := run.Error
		s.mu.Unlock()
		// 终态事件: SSE 订阅方靠它知道"流结束了, 可以关连接" (而不是靠连接被
		// 掐断 —— 掐断与正常结束在 EventSource 上都是 onerror, 分不开)。
		run.events.append(Event{Kind: EventRunDone, Status: string(final), Error: errMsg, At: time.Now()})
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"runId":     run.ID,
		"sessionId": id,
		"status":    RunRunning,
	})
}

// postCancel POST /api/sessions/{id}/runs/{runId}/cancel —— 取消一次正在跑的 run。
//
// 幂等: 已到终态的 run 返回 200 + {cancelled:false, status}, 不是 409 ——
// "取消一个已经结束的东西"不是冲突, 是什么都不用做。
func (s *Server) postCancel(w http.ResponseWriter, sessionID, runID string) {
	run := s.findRun(sessionID, runID)
	if run == nil {
		writeErr(w, http.StatusNotFound, "没有这次运行: "+runID)
		return
	}
	s.mu.Lock()
	cancel := run.cancel
	s.mu.Unlock()
	if cancel == nil {
		writeJSON(w, http.StatusOK, map[string]any{"cancelled": false, "status": run.Status})
		return
	}
	cancel()
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}

// postInject POST /api/sessions/{id}/inject —— 运行中插话 (G3 的另一半)。
//
// 与 POST messages 的分工: messages 开**新一轮** (会话忙时 409), inject 把话塞进
// **当前这一轮** (下一个轮次边界被模型读到)。考场"打断并纠正 AI 正在做的事"用它。
func (s *Server) postInject(w http.ResponseWriter, r *http.Request, h Host, id string) {
	if _, err := h.Get(id); err != nil {
		s.writeHostErr(w, err)
		return
	}
	ih, ok := h.(InjectHost)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "本会话运行时不支持运行中插话")
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeErr(w, http.StatusBadRequest, "text 不能为空")
		return
	}
	if !ih.Inject(id, body.Text) {
		// 没有循环在跑 = 没人接。如实 409, 调用方应改走 POST messages 开新一轮。
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "会话当前没有在跑的运行, 注入无人接收; 请改用 POST messages",
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"injected": true})
}

// handleTrace GET /api/traces/{runId} —— 导出一个 run 的结构化轨迹 (G2)。
//
// span 的 InputRef/OutputRef 一并解析成文本 (截断 capTraceBodyBytes), 调用方拿到的
// 就是一份自包含的评价素材, 不需要知道 blob 存储的存在。
func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	runID := strings.TrimPrefix(r.URL.Path, "/api/traces/")
	runID = strings.Trim(runID, "/")
	if runID == "" {
		writeErr(w, http.StatusNotFound, "缺少 runId")
		return
	}
	h := s.host()
	if h == nil {
		writeErr(w, http.StatusServiceUnavailable, "会话运行时尚未就绪")
		return
	}
	th, ok := h.(TraceHost)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "本会话运行时不支持轨迹导出")
		return
	}
	spans, err := th.Trace(runID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if spans == nil {
		spans = []tracestore.Span{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runId": runID, "spans": spans})
}

// runView 把 Run 渲染成对外形态, 附带 token 消耗。
func (s *Server) runView(h Host, run *Run) map[string]any {
	s.mu.RLock()
	out := map[string]any{
		"runId":     run.ID,
		"sessionId": run.SessionID,
		"status":    run.Status,
		"startedAt": run.StartedAt,
	}
	if run.Reply != "" {
		out["reply"] = run.Reply
	}
	if run.Error != "" {
		out["error"] = run.Error
	}
	if !run.EndedAt.IsZero() {
		out["endedAt"] = run.EndedAt
		out["elapsedMs"] = run.EndedAt.Sub(run.StartedAt).Milliseconds()
	}
	s.mu.RUnlock()

	// token 是**独立**读的 (读轨迹底座), 不在锁里 —— 它是 I/O, 不该占着运行表的锁。
	// 只给终态: 还在跑的时候 token 是半截数, 报出去会被当成最终值。
	if run.Status != RunRunning {
		if u, err := h.Tokens(run.ID); err == nil && u != nil {
			out["tokens"] = u
		}
	}
	return out
}

// writeHostErr 把 Host 返回的错误映射成 404 (未找到) 或 500。
func (s *Server) writeHostErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "没有这个会话")
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}

// ── run 档案表 ──────────────────────────────────────────────────────────────

// remember 把一个 run 记进会话档案表头部, 并按 maxRetainedRuns 截断尾部。
func (s *Server) remember(run *Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := append([]*Run{run}, s.runs[run.SessionID]...)
	if len(list) > maxRetainedRuns {
		list = list[:maxRetainedRuns]
	}
	s.runs[run.SessionID] = list
}

// findRun 在会话档案表里按 runID 查 run; 查不到返回 nil。
func (s *Server) findRun(sessionID, runID string) *Run {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.runs[sessionID] {
		if r.ID == runID {
			return r
		}
	}
	return nil
}

// lastRun 返回该会话最近一次 run (档案表头); 没有则返回 nil。
func (s *Server) lastRun(sessionID string) *Run {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if list := s.runs[sessionID]; len(list) > 0 {
		return list[0]
	}
	return nil
}

// runsFor 返回该会话的 run 列表副本 (新的在前)。
func (s *Server) runsFor(sessionID string) []*Run {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Run, len(s.runs[sessionID]))
	copy(out, s.runs[sessionID])
	return out
}

// runningRunIDs 返回该会话中仍处于 running 状态的 run id。
func (s *Server) runningRunIDs(sessionID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, r := range s.runs[sessionID] {
		if r.Status == RunRunning {
			out = append(out, r.ID)
		}
	}
	return out
}

// recentRunIDs 返回最近 n 个 run id (不足 n 个则全部)。
func (s *Server) recentRunIDs(sessionID string, n int) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.runs[sessionID]
	if len(list) < n {
		n = len(list)
	}
	out := make([]string, 0, n)
	for _, r := range list[:n] {
		out = append(out, r.ID)
	}
	return out
}

// TokensFromTrace 是 Host.Tokens 的一个现成实现: 从轨迹底座累加 llm_call 的 token。
//
// 之所以放在本包: 它是"从哪里读 token"这件事的**唯一**答案, 而实现方
// (pkg/feishu) 不必再抄一遍。采不到返回 (nil, nil) —— 那是"没采集", 不是 0。
func TokensFromTrace(store *tracestore.Store, runID string) (*Usage, error) {
	if store == nil {
		return nil, nil
	}
	spans, err := store.ReadRun(runID)
	if err != nil {
		return nil, nil // 读不到 = 没采集, 不向上报错 (轮询不该因为采不到而失败)
	}
	u := &Usage{}
	found := false
	for _, sp := range spans {
		if sp.Kind != tracestore.KindLLMCall {
			continue
		}
		found = true
		u.Calls++
		u.In += intAttr(sp.Attrs, "input_tokens")
		u.Out += intAttr(sp.Attrs, "output_tokens")
		u.CacheRead += intAttr(sp.Attrs, "cache_read_tokens")
		u.CacheCreate += intAttr(sp.Attrs, "cache_creation_tokens")
	}
	if !found {
		return nil, nil // 一条 llm_call 都没有 = 没采到, 而不是"跑了 0 次调用"
	}
	return u, nil
}

// intAttr 从 attrs 里取一个整数。JSON 往返会把数字变成 float64, 两种都要认。
func intAttr(attrs map[string]any, key string) int {
	switch v := attrs[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// firstOrEmpty 取字符串切片首元素; 空切片返回空串。
func firstOrEmpty(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

// ── 响应helpers ─────────────────────────────────────────────────────────────

// writeJSON 以 UTF-8 JSON 写响应。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr 写一个 {"error": msg} 响应。
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

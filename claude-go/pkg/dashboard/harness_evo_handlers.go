package dashboard

// harness_evo_handlers.go — HarnessPro 论文进化运行的 dashboard 控制面。
//
//	POST /api/harness-evo/runs              触发一次进化运行
//	GET  /api/harness-evo/runs              列出运行 (新→旧)
//	GET  /api/harness-evo/runs/{id}         运行详情 (状态/进度/事件流/报告)
//	POST /api/harness-evo/runs/{id}/stop    停止运行 (取消 ctx)
//	GET  /api/harness-evo/runs/{id}/files            产物清单 (workdir 文件树 + git log)
//	GET  /api/harness-evo/runs/{id}/files?path=<rel> 单个产物内容 (文本直出/二进制 base64)
//
// 运行在本进程 (control :18080) 内**进程内**执行 pkg/harnesspro 编排器 ——
// 与 `claude-go harness-evo run` CLI 同一引擎, 但事件流/进度/报告经本控制面
// 暴露, 供 webapp 工作流轮询消费 (webapp 的 team 轮询协议覆盖不到这种
// "非团队运行时", 故这里给一套平行的小型 run 协议)。
//
// LLM 客户端按请求里的 model 别名经 providers 配置解析 (与 buildEngine
// 同一条 resolveRuntimeModelConfig 路径, resolver 内部已含网关改指);
// 解析不到时回退 GetSharedLLMClient。运行状态同时落盘
// <StateDir>/harness-evo/runs/<id>/status.json —— 控制面重启后
// 列表/详情仍能看到历史运行 (事件流只保留尾部)。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/harnesspro"
)

// ─── 参数与状态结构 ────────────────────────────────────────────────────────────

// harnessEvoParams 一次进化运行的可调参数 (全部可选, 零值 = 默认值)。
type harnessEvoParams struct {
	Model       string            `json:"model,omitempty"`        // 模型别名 (如 opencode:deepseek-v4.1-flash); 空 = 配置默认
	Epochs      int               `json:"epochs,omitempty"`       // 进化轮次 T (默认 2; 论文 3-4)
	K           int               `json:"k,omitempty"`            // 训练集 rollout 重跑次数 (默认 3)
	TrainN      int               `json:"train,omitempty"`        // 训练集规模 (默认 10)
	TimeoutMin  int               `json:"timeout_min,omitempty"`  // 总超时分钟 (默认 60)
	MaxParallel int               `json:"max_parallel,omitempty"` // 图内并发上限 (默认 6)
	Tiers       string            `json:"tiers,omitempty"`        // 三档权重 lite:1,3|medium:1,1|heavy:3,1
	Resume      bool              `json:"resume,omitempty"`       // 从 journal 续跑
	Pool        []harnesspro.Task `json:"pool,omitempty"`         // 自定义任务池 (空 = 内置 12 题; 与 taskset 互斥)
	TaskSet     string            `json:"taskset,omitempty"`      // 已注册任务集 name[@version]
	H0          string            `json:"h0,omitempty"`           // 自定义基座 harness 指令 (空 = 内置)
}

// normalize 缺省补齐 (与 CLI 旗标默认一致; webapp 触发时传冒烟档)。
func (p harnessEvoParams) normalize() harnessEvoParams {
	if p.Epochs <= 0 {
		p.Epochs = 2
	}
	if p.K <= 0 {
		p.K = 3
	}
	if p.TrainN <= 0 {
		p.TrainN = 10
	}
	if p.TimeoutMin <= 0 {
		p.TimeoutMin = 60
	}
	if p.MaxParallel <= 0 {
		p.MaxParallel = 6
	}
	return p
}

// harnessEvoEvent 一条进度事件 (notify 消息的结构化形态)。
type harnessEvoEvent struct {
	Seq     int64  `json:"seq"`
	Time    int64  `json:"time"`
	Stage   string `json:"stage,omitempty"`
	Message string `json:"message"`
}

// harnessEvoRun 一次进化运行的完整状态 (内存态 + 落盘态同构)。
type harnessEvoRun struct {
	ID          string                      `json:"id"`
	Params      harnessEvoParams            `json:"params"`
	Status      string                      `json:"status"` // running|completed|failed|stopped
	Stage       string                      `json:"stage"`
	Progress    int                         `json:"progress"`
	Events      []harnessEvoEvent           `json:"events"`
	Report      *harnesspro.EvolutionReport `json:"report,omitempty"`
	TaskSet     harnesspro.TaskSetRef       `json:"task_set"`
	Error       string                      `json:"error,omitempty"`
	Workdir     string                      `json:"workdir"`
	StartedAt   int64                       `json:"started_at"`
	CompletedAt int64                       `json:"completed_at,omitempty"`
}

// deriveStage 从编排器 notify 消息推导 (stage, progress)。
// 消息词表是 pkg/harnesspro 的稳定产物 (orchestrator/runner 里的中文锚点),
// 未命中的消息保留当前 stage —— 进度条允许粗糙, 事件流才是真相。
func deriveHarnessEvoStage(msg string, epochs int) (string, int) {
	switch {
	case strings.HasPrefix(msg, "选题"):
		return "选题", 5
	case strings.HasPrefix(msg, "selfcheck"), strings.HasPrefix(msg, "出卷自检"),
		strings.HasPrefix(msg, "新候选"), strings.HasPrefix(msg, "动刀"):
		return "出卷/进化图执行", 15
	case strings.HasPrefix(msg, "地板"):
		return "地板评定", 25
	}
	// "epoch N 完成, 晋级: ..." / "epoch N 选点: ..."
	if strings.HasPrefix(msg, "epoch ") {
		rest := msg[len("epoch "):]
		n := 0
		for i, c := range rest {
			if c < '0' || c > '9' {
				rest = rest[:i]
				break
			}
		}
		n, _ = strconv.Atoi(rest)
		if n > 0 {
			if epochs <= 0 {
				epochs = 1
			}
			prog := 25 + (70 * n / epochs)
			if prog > 98 {
				prog = 98
			}
			if strings.Contains(msg, "选点") {
				return fmt.Sprintf("epoch %d 选点", n), prog - 35/epochs
			}
			return fmt.Sprintf("epoch %d 完成", n), prog
		}
	}
	return "", 0
}

// ─── 运行管理器 ────────────────────────────────────────────────────────────────

type harnessEvoManager struct {
	mu      sync.Mutex
	runs    map[string]*harnessEvoRun
	cancels map[string]context.CancelFunc
	root    string // <StateDir>/harness-evo/runs
}

var (
	evoMgrOnce sync.Once
	evoMgr     *harnessEvoManager
)

// sharedHarnessEvoManager 惰性构造单例 (StateDir 在 Server 构造后必定可知)。
func sharedHarnessEvoManager(stateDir string) *harnessEvoManager {
	evoMgrOnce.Do(func() {
		root := filepath.Join(stateDir, "harness-evo", "runs")
		_ = os.MkdirAll(root, 0o755)
		evoMgr = &harnessEvoManager{
			runs:    map[string]*harnessEvoRun{},
			cancels: map[string]context.CancelFunc{},
			root:    root,
		}
		evoMgr.loadFromDisk()
	})
	return evoMgr
}

func (m *harnessEvoManager) taskSetDir() string {
	return filepath.Join(filepath.Dir(m.root), "tasksets")
}

func (m *harnessEvoManager) versionDir() string {
	return filepath.Join(filepath.Dir(m.root), "versions")
}

// statusPath 运行的落盘状态文件。
func (m *harnessEvoManager) statusPath(id string) string {
	return filepath.Join(m.root, id, "status.json")
}

// loadFromDisk 启动时回扫历史运行 (控制面重启后列表不丢; running 态名归 failed
// —— 进程退出即 ctx 死亡, 没人会再写它的终态, 诚实标记)。
func (m *harnessEvoManager) loadFromDisk() {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(m.statusPath(e.Name()))
		if err != nil {
			continue
		}
		var r harnessEvoRun
		if json.Unmarshal(b, &r) != nil {
			continue
		}
		if r.Status == "running" {
			r.Status = "failed"
			r.Error = "控制面重启, 运行中断 (进程内执行不跨重启续跑, 可用 resume 重触发)"
			r.CompletedAt = time.Now().Unix()
		}
		m.runs[r.ID] = &r
	}
}

// activeRun 当前是否有运行在进行 (进化烧钱, 同时只放行一个)。
func (m *harnessEvoManager) activeRun() *harnessEvoRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.runs {
		if r.Status == "running" {
			return r
		}
	}
	return nil
}

// start 触发一次运行 (异步)。已有活动运行时返回 409 语义错误。
func (m *harnessEvoManager) start(params harnessEvoParams) (*harnessEvoRun, error) {
	if act := m.activeRun(); act != nil {
		return nil, fmt.Errorf("已有进化运行进行中 (%s, stage=%s), 请等待完成或先停止", act.ID, act.Stage)
	}
	params = params.normalize()
	var taskSetRef harnesspro.TaskSetRef
	if strings.TrimSpace(params.TaskSet) != "" {
		if len(params.Pool) > 0 {
			return nil, fmt.Errorf("taskset 与 pool 互斥, 请选择一种任务来源")
		}
		set, err := harnesspro.NewTaskSetStore(m.taskSetDir()).Get(params.TaskSet)
		if err != nil {
			return nil, err
		}
		params.Pool = set.Tasks
		params.TaskSet = set.Name + "@" + set.Version
		taskSetRef = set.Ref()
	}
	if err := validateHarnessEvoPool(params.Pool); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("evo-%d-%04d", time.Now().Unix(), time.Now().Nanosecond()%10000)
	workdir := filepath.Join(m.root, id, "wd")
	r := &harnessEvoRun{
		ID:        id,
		Params:    params,
		Status:    "running",
		Stage:     "初始化",
		Progress:  2,
		Workdir:   workdir,
		TaskSet:   taskSetRef,
		StartedAt: time.Now().Unix(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(params.TimeoutMin)*time.Minute)

	m.mu.Lock()
	m.runs[id] = r
	m.cancels[id] = cancel
	m.mu.Unlock()
	m.persist(r)

	go m.run(ctx, r)
	return r, nil
}

// run 执行体: 解析 LLM → 构造编排器 → Run → 归终态。
func (m *harnessEvoManager) run(ctx context.Context, r *harnessEvoRun) {
	finish := func(status, errMsg string, report *harnesspro.EvolutionReport) {
		m.mu.Lock()
		r.Status = status
		r.Error = errMsg
		r.Report = report
		r.CompletedAt = time.Now().Unix()
		if status == "completed" {
			r.Progress = 100
			r.Stage = "完成"
		}
		m.mu.Unlock()
		m.push(r, "收尾", fmt.Sprintf("运行结束: status=%s", status))
		m.persist(r)
	}

	client, err := resolveHarnessEvoClient(r.Params.Model)
	if err != nil {
		finish("failed", "LLM 客户端解析失败: "+err.Error(), nil)
		return
	}
	cfg := harnesspro.Config{
		RunID: r.ID, Epochs: r.Params.Epochs, K: r.Params.K, TrainN: r.Params.TrainN,
		Workdir: r.Workdir, VersionDir: m.versionDir(), TaskSet: r.TaskSet,
		Resume: r.Params.Resume, MaxParallel: r.Params.MaxParallel,
	}
	if len(r.Params.Pool) > 0 {
		cfg.Pool = r.Params.Pool
	}
	if strings.TrimSpace(r.Params.H0) != "" {
		cfg.H0 = r.Params.H0
	}
	if r.Params.Tiers != "" {
		tiers, err := parseHarnessEvoTiers(r.Params.Tiers)
		if err != nil {
			finish("failed", err.Error(), nil)
			return
		}
		cfg.Tiers = tiers
	}
	orch, err := harnesspro.NewOrchestrator(cfg, &harnesspro.APILLM{Client: client})
	if err != nil {
		finish("failed", err.Error(), nil)
		return
	}
	m.push(r, "初始化", fmt.Sprintf("开始进化: epochs=%d k=%d train=%d model=%s",
		cfg.Epochs, cfg.K, cfg.TrainN, client.Model))

	report, err := orch.Run(ctx, func(msg string) {
		stage, prog := deriveHarnessEvoStage(msg, cfg.Epochs)
		m.mu.Lock()
		if stage != "" {
			r.Stage = stage
		}
		if prog > r.Progress {
			r.Progress = prog
		}
		cur := r.Stage
		m.mu.Unlock()
		m.push(r, cur, msg)
	})
	if err != nil {
		if ctx.Err() != nil {
			finish("stopped", "已停止: "+ctx.Err().Error(), report)
			return
		}
		finish("failed", err.Error(), report)
		return
	}
	finish("completed", "", report)
}

// push 追加事件 (尾部 500 条封顶) 并落盘。
func (m *harnessEvoManager) push(r *harnessEvoRun, stage, msg string) {
	m.mu.Lock()
	r.Events = append(r.Events, harnessEvoEvent{
		Seq:     int64(len(r.Events)) + 1,
		Time:    time.Now().Unix(),
		Stage:   stage,
		Message: msg,
	})
	if len(r.Events) > 500 {
		r.Events = r.Events[len(r.Events)-500:]
	}
	m.mu.Unlock()
	m.persist(r)
}

// persist 落盘状态 (best-effort; 事件截尾 500 与内存一致)。
func (m *harnessEvoManager) persist(r *harnessEvoRun) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(m.statusPath(r.ID)), 0o755)
	tmp := m.statusPath(r.ID) + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, m.statusPath(r.ID))
	}
}

// get 详情 (拷贝, 避免调用方读到并发写)。
func (m *harnessEvoManager) get(id string) *harnessEvoRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return nil
	}
	cp := *r
	return &cp
}

// list 列表 (新→旧, 内存运行优先, 落盘历史并集)。
func (m *harnessEvoManager) list() []*harnessEvoRun {
	m.mu.Lock()
	out := make([]*harnessEvoRun, 0, len(m.runs))
	for _, r := range m.runs {
		cp := *r
		out = append(out, &cp)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}

// stop 取消运行 ctx。
func (m *harnessEvoManager) stop(id string) error {
	m.mu.Lock()
	cancel, ok := m.cancels[id]
	r := m.runs[id]
	m.mu.Unlock()
	if !ok || r == nil {
		return fmt.Errorf("运行不存在: %s", id)
	}
	if r.Status != "running" {
		return fmt.Errorf("运行不在进行中 (status=%s)", r.Status)
	}
	cancel()
	return nil
}

// ─── LLM 客户端解析 ────────────────────────────────────────────────────────────

// resolveHarnessEvoClient 按模型别名构造 api.Client —— 与 cmd/claude-go
// buildEngine 的 providers 解析同路径 (resolver 内部已做网关改指与 ViaGateway 标记);
// 配置文件缺失或别名未命中时回退 dashboard 共享客户端 (配置默认模型)。
func resolveHarnessEvoClient(model string) (*api.Client, error) {
	if jsonCfg, _, err := loadFeishuJSONConfig(); err == nil && jsonCfg != nil && len(jsonCfg.Providers) > 0 {
		cfgJSON := jsonCfg.ToModelConfigJSON()
		model = strings.TrimSpace(model)
		if model != "" && model != "default" && strings.Contains(model, ":") {
			cfgJSON.AI.GlobalConfig.DefaultModelAlias = model
			cfgJSON.AI.GlobalConfig.DefaultFallbackAliases = nil
			cfgJSON.AI.Plans = nil
		}
		if _, resolver, err := modelconfig.LoadFromConfig(cfgJSON); err == nil && resolver != nil {
			resolved := resolver.Resolve("", "")
			if resolved.APIKey != "" && resolved.BaseURL != "" {
				return harnessEvoClientFromResolved(resolved), nil
			}
		}
	}
	client, _, err := GetSharedLLMClient()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(model) != "" && model != "default" {
		if c := client.WithModel(model); c != nil {
			return c, nil
		}
	}
	return client, nil
}

// harnessEvoClientFromResolved 照抄 buildEngine 的客户端构造字段 (main.go
// buildEngine 段): 出站模型名/协议路由/模型级代理与超时/限流守卫 —— 缺一项
// 就会在某类 provider 上复现 main.go 注释里那些 400/超时, 这里保持同构。
func harnessEvoClientFromResolved(resolved modelconfig.ResolvedConfig) *api.Client {
	baseURL := strings.TrimRight(resolved.BaseURL, "/")
	if strings.HasSuffix(baseURL, "/anthropic") || strings.HasSuffix(baseURL, "/compatible-mode") {
		baseURL += "/v1"
	}
	var c *api.Client
	if api.IsLocalEndpoint(baseURL) {
		c = api.NewOllamaClient(baseURL, resolved.PreferredModelName())
		c.APIKey = resolved.APIKey
	} else {
		c = api.NewClient(baseURL, resolved.APIKey, resolved.PreferredModelName())
	}
	c.Model = resolved.OutboundModelName()
	c.FallbackModels = resolved.FallbackModels
	c.FallbackBaseURL = resolved.FallbackBaseURL
	c.FallbackAPIKey = resolved.FallbackAPIKey
	c.PromptCacheMode = resolved.PromptCacheMode
	c.Protocol = resolved.Protocol
	c.ReasoningPlayback = resolved.ReasoningPlayback
	c.SetProxy(resolved.Proxy)
	if resolved.FirstTokenTimeoutSec > 0 {
		c.FirstTokenTimeout = time.Duration(resolved.FirstTokenTimeoutSec) * time.Second
	}
	if resolved.CallTimeoutSec > 0 {
		c.CallTimeout = time.Duration(resolved.CallTimeoutSec) * time.Second
	}
	if resolved.DeadlineRetryBaseSec > 0 {
		c.DeadlineRetryBase = time.Duration(resolved.DeadlineRetryBaseSec) * time.Second
	}
	guardCfg := api.DefaultGuardConfig()
	if resolved.RPM > 0 {
		guardCfg.RPM = resolved.RPM
	}
	if resolved.MaxParallel > 0 {
		guardCfg.MaxParallel = resolved.MaxParallel
	}
	if resolved.MinParallel > 0 {
		guardCfg.MinParallel = resolved.MinParallel
	}
	c.Guard = api.NewRateLimitGuard(guardCfg)
	c.Tag = "harness-evo"
	return c
}

// parseHarnessEvoPool 把 JSON 解码出的任意值 (webapp payload / objective 里的
// pool 数组) round-trip 成任务池。结构不合法给可读的 400 而不是静默忽略。
func parseHarnessEvoPool(v any) ([]harnesspro.Task, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("pool 序列化失败: %v", err)
	}
	var pool []harnesspro.Task
	if err := json.Unmarshal(b, &pool); err != nil {
		return nil, fmt.Errorf("pool 结构错误 (应为 [{id,category,difficulty,prompt,...}...]): %v", err)
	}
	if err := validateHarnessEvoPool(pool); err != nil {
		return nil, err
	}
	return pool, nil
}

// validateHarnessEvoPool 自定义任务池校验 (webapp JSON 表单 / 直连 API 都可能喂进来):
// 每题必须有 id+prompt, id 唯一, 规模至少 2 题 (否则训练/验证切不开)。
func validateHarnessEvoPool(pool []harnesspro.Task) error {
	if len(pool) == 0 {
		return nil // 空 = 用内置池
	}
	return harnesspro.ValidateTaskPool(pool)
}

// parseHarnessEvoTiers 解析三档权重 (形如 lite:1,3|medium:1,1|heavy:3,1)。
func parseHarnessEvoTiers(s string) ([]harnesspro.TierWeight, error) {
	var out []harnesspro.TierWeight
	for _, part := range strings.Split(s, "|") {
		name, rest, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok || name == "" {
			return nil, fmt.Errorf("tiers 格式错误 (应为 lite:1,3|medium:1,1|heavy:3,1): %q", part)
		}
		aStr, bStr, ok := strings.Cut(rest, ",")
		if !ok {
			return nil, fmt.Errorf("tiers 权重缺少 β: %q", part)
		}
		alpha, err := strconv.ParseFloat(strings.TrimSpace(aStr), 64)
		if err != nil {
			return nil, fmt.Errorf("tiers α 解析失败: %q", part)
		}
		beta, err := strconv.ParseFloat(strings.TrimSpace(bStr), 64)
		if err != nil {
			return nil, fmt.Errorf("tiers β 解析失败: %q", part)
		}
		out = append(out, harnesspro.TierWeight{Tier: name, Alpha: alpha, Beta: beta})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("tiers 为空")
	}
	return out, nil
}

// ─── HTTP handlers ─────────────────────────────────────────────────────────────

// handleHarnessEvoRuns POST 触发 / GET 列表。
func (s *Server) handleHarnessEvoRuns(w http.ResponseWriter, r *http.Request) {
	mgr := sharedHarnessEvoManager(s.cfg.StateDir)
	switch r.Method {
	case http.MethodPost:
		var params harnessEvoParams
		if r.Body != nil {
			defer r.Body.Close()
			_ = json.NewDecoder(r.Body).Decode(&params) // 空体 = 全默认
		}
		run, err := mgr.start(params)
		if err != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("%v", err))
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": run.ID, "status": run.Status, "workdir": run.Workdir, "started_at": run.StartedAt,
		})
	case http.MethodGet:
		writeJSON(w, http.StatusOK, mgr.list())
	default:
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST/GET required"))
	}
}

// handleHarnessEvoRun 单个运行的详情 / 停止。路径 /api/harness-evo/runs/{id}[/stop]。
func (s *Server) handleHarnessEvoRun(w http.ResponseWriter, r *http.Request) {
	mgr := sharedHarnessEvoManager(s.cfg.StateDir)
	rest := strings.TrimPrefix(r.URL.Path, "/api/harness-evo/runs/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("path must be /api/harness-evo/runs/{id}[/stop]"))
		return
	}
	id := parts[0]
	run := mgr.get(id)
	if run == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("运行不存在: %s", id))
		return
	}
	if len(parts) == 2 && parts[1] == "stop" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
			return
		}
		if err := mgr.stop(id); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "stopping": true})
		return
	}
	if len(parts) == 2 && parts[1] == "events" && r.URL.Query().Get("after") != "" {
		// 增量事件: ?after=<seq> 只回新事件 (轮询方 dedupe 用)。
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		var fresh []harnessEvoEvent
		for _, ev := range run.Events {
			if ev.Seq > after {
				fresh = append(fresh, ev)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "events": fresh, "status": run.Status})
		return
	}
	if len(parts) == 2 && parts[1] == "files" {
		// 产物直查: workdir 文件树 / 单文件内容。webapp 详情页"进化产物"面板的数据源。
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("GET required"))
			return
		}
		s.handleHarnessEvoFiles(w, r, run)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("GET required"))
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// ─── 产物直查 ──────────────────────────────────────────────────────────────────

// harnessEvoFileEntry 产物清单里的一条 (workdir 内相对路径)。
type harnessEvoFileEntry struct {
	Path string `json:"path"`             // 相对 workdir 的路径 (POSIX 分隔)
	Size int64  `json:"size"`             // 字节数
	Bin  bool   `json:"binary,omitempty"` // 首 8KB 含 NUL → 前端不内联渲染
}

// handleHarnessEvoFiles GET /api/harness-evo/runs/{id}/files[?path=<rel>]。
// 不带 path: 递归列 workdir 文件 (跳过 .git 内部) + git log --oneline;
// 带 path: 返回单个文件内容 (≤512KB, 文本/ base64)。
func (s *Server) handleHarnessEvoFiles(w http.ResponseWriter, r *http.Request, run *harnessEvoRun) {
	rel := r.URL.Query().Get("path")
	if rel != "" {
		s.writeHarnessEvoFileContent(w, run, rel)
		return
	}

	entries, err := harnessEvoListFiles(run.Workdir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      run.ID,
		"workdir": run.Workdir,
		"files":   entries,
		"git_log": harnessEvoGitLog(run.Workdir),
	})
}

// harnessEvoSafeJoin workdir 内的路径拼接 + 越界防护 (拒绝绝对路径与 .. 逃逸)。
func harnessEvoSafeJoin(workdir, rel string) (string, error) {
	if strings.ContainsRune(rel, '\\') || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("非法路径: %q", rel)
	}
	full := filepath.Join(workdir, filepath.Clean(rel))
	if full != workdir && !strings.HasPrefix(full, workdir+string(os.PathSeparator)) {
		return "", fmt.Errorf("路径越界: %q", rel)
	}
	return full, nil
}

// harnessEvoIsBinary 首 8KB 含 NUL 即视为二进制 (journal.jsonl 等都是文本)。
func harnessEvoIsBinary(b []byte) bool {
	const sniff = 8192
	if len(b) > sniff {
		b = b[:sniff]
	}
	return strings.IndexByte(string(b), 0) >= 0
}

// harnessEvoListFiles 递归列 workdir 文件 (跳过 .git 内部), 按路径排序, 封顶 500。
func harnessEvoListFiles(workdir string) ([]harnessEvoFileEntry, error) {
	var out []harnessEvoFileEntry
	err := filepath.WalkDir(workdir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 权限/瞬删的文件跳过, 不让单点失败拖垮整单
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(workdir, path)
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		full, err := os.ReadFile(path) // 产物都小 (report.json ~8K, journal ~120K), 直接嗅探
		bin := err == nil && harnessEvoIsBinary(full)
		out = append(out, harnessEvoFileEntry{
			Path: filepath.ToSlash(rel),
			Size: info.Size(),
			Bin:  bin,
		})
		if len(out) >= 500 {
			return fmt.Errorf("stop")
		}
		return nil
	})
	if err != nil && err.Error() != "stop" {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// harnessEvoGitLog workdir 的候选提交历史 (每候选一个 commit 是论文形态的核心产物)。
func harnessEvoGitLog(workdir string) string {
	if _, err := os.Stat(filepath.Join(workdir, ".git")); err != nil {
		return ""
	}
	out, err := exec.Command("git", "-C", workdir, "log", "--oneline", "--decorate", "-n", "50").Output()
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(out), "\n")
}

// writeHarnessEvoFileContent 单文件内容: ≤512KB, 文本直出 / 二进制 base64。
func (s *Server) writeHarnessEvoFileContent(w http.ResponseWriter, run *harnessEvoRun, rel string) {
	full, err := harnessEvoSafeJoin(run.Workdir, rel)
	if err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, fmt.Errorf("产物不存在: %q", rel))
		return
	}
	const cap = 512 * 1024
	b, err := os.ReadFile(full)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	truncated := false
	if len(b) > cap {
		b = b[:cap]
		truncated = true
	}
	resp := map[string]any{
		"path": rel, "size": info.Size(), "truncated": truncated,
	}
	if harnessEvoIsBinary(b) {
		resp["encoding"] = "base64"
		resp["content"] = base64.StdEncoding.EncodeToString(b)
	} else {
		resp["encoding"] = "text"
		resp["content"] = string(b)
	}
	writeJSON(w, http.StatusOK, resp)
}

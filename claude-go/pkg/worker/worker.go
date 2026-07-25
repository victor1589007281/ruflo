package worker

// worker.go —— worker 侧: 注册/心跳/拉任务/**用既有本地执行路径真跑**/续租/
// 回传事件/回报终态。
//
// 与改造前 `cmd/claude-go/main.go` 里那个循环的区别只有一处但是全部意义所在:
// 执行体不再是 `executeWorkerTask` 的 payload 回显, 而是一个真的
// `agent.AgentRuntime` (生产接线 = `agent.NewLocalRuntime(name, caps,
// feishu.SessionManager.CreateAgentRunner)`, 即 QueryEngine + prompt 组装 +
// skills + 工具画像 + MCP 全套)。本包不自造任何 agent 执行逻辑。
//
// 五条"失败必须真失败"的处理 (逐条都有测试):
//
//	载荷解码失败      → Fail(原因)      ——不许拿空 prompt 跑出"成功的垃圾"
//	能力不匹配        → Fail(缺哪些)    ——纵深防御, 队列过滤失灵时不硬跑
//	工作区无法提供    → Fail(要哪个/有哪个) ——不许在错误目录里产码
//	runtime 未给终态  → Fail(未回报终态) ——通道关了却没 done/failed 不算成功
//	worker 关停       → Fail(关停)      ——在途任务立刻让控制面看到, 不等租约超时

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/cluster"
	"github.com/anthropic/claude-go/pkg/trace"
)

// 默认参数。
const (
	DefaultPollMS           = 1000
	DefaultHeartbeatEvery   = 30 * time.Second
	DefaultKeepaliveEvery   = 15 * time.Second // 事件冲刷 + 续租 + 取回取消指令
	DefaultReportRetries    = 3                // 终态回报的重试次数 (丢一次成功结果代价很高)
	DefaultEventBatchMaxLen = 32
)

// Options worker 构造参数。
type Options struct {
	// Control 必填: 控制面基址 (如 http://claude-go-control:18080)。
	Control string
	// Name 必填: worker 名 (注册表主键, 也是钉住标签 worker:<name> 的内容)。
	Name string
	// Runtime 必填: 真执行体。生产传 agent.NewLocalRuntime(...)。
	Runtime agent.AgentRuntime
	// ExtraCaps 额外能力标签 (与 Runtime.Capabilities() 的投影合并)。
	// 用于声明 Runtime 结构体表达不了的本地资源, 如 mcp:playwright / cli:golangci-lint。
	ExtraCaps []string
	// Kinds 接受的任务 Kind, 空 → ["stage"]。
	Kinds []string
	// Workspace 本 worker 能提供的团队工作区 (绝对路径)。空 = 不声明,
	// 此时任何指定了 Workspace 的任务都会被拒 (fail-closed, 见 checkWorkspace)。
	Workspace string
	// MaxParallel 并发执行上限, <=0 → 1。
	MaxParallel int
	// PollInterval 无任务时的轮询间隔, <=0 → DefaultPollMS。
	PollInterval time.Duration
	// HeartbeatInterval <=0 → DefaultHeartbeatEvery。必须显著小于控制面注册表租约。
	HeartbeatInterval time.Duration
	// KeepaliveInterval <=0 → DefaultKeepaliveEvery。同时决定跨进程取消的延迟上限。
	KeepaliveInterval time.Duration
	// EventsPath 事件回传路径, 空 → DefaultEventsPath。
	EventsPath string
	// AuthToken 控制面 Bearer token。
	//
	// 必须有: 控制面的 httpauth 中间件默认保护 /cluster/ 前缀
	// (pkg/httpauth/httpauth.go:36 DefaultProtectPrefixes), 配了 wiki.apiSecret
	// 的部署下不带 token 的 worker 会在每次拉取上拿到 401。空 = 不带头
	// (控制面未设 secret 时的既有行为)。
	AuthToken string
	// HTTPClient 可空。
	HTTPClient *http.Client
	// Logf 可空 → 丢弃。
	Logf func(format string, args ...any)
}

// Worker 一个长跑的远程 Agent 运行时。
type Worker struct {
	opt   Options
	cli   *ctlClient
	caps  []string
	kinds []string

	mu   sync.Mutex
	done int
	fail int
}

// New 构造 worker。缺少 Control/Name/Runtime 直接报错 —— 一个连不上控制面或没有
// 执行体的 worker 只会安静地什么都不做, 那正是改造前的状态。
func New(opt Options) (*Worker, error) {
	if strings.TrimSpace(opt.Control) == "" {
		return nil, fmt.Errorf("worker: Options.Control (控制面基址) 不能为空")
	}
	if strings.TrimSpace(opt.Name) == "" {
		return nil, fmt.Errorf("worker: Options.Name 不能为空")
	}
	if opt.Runtime == nil {
		return nil, fmt.Errorf("worker: Options.Runtime 不能为空 (需要真执行体, 不接受桩)")
	}
	if opt.MaxParallel <= 0 {
		opt.MaxParallel = 1
	}
	if opt.PollInterval <= 0 {
		opt.PollInterval = DefaultPollMS * time.Millisecond
	}
	if opt.HeartbeatInterval <= 0 {
		opt.HeartbeatInterval = DefaultHeartbeatEvery
	}
	if opt.KeepaliveInterval <= 0 {
		opt.KeepaliveInterval = DefaultKeepaliveEvery
	}
	if opt.EventsPath == "" {
		opt.EventsPath = DefaultEventsPath
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	kinds := opt.Kinds
	if len(kinds) == 0 {
		kinds = []string{TaskKindStage}
	}
	// 能力标签 = runtime 自己声明的能力 + 额外标签 + 钉住自己的合成标签。
	// 以 Runtime.Capabilities() 为准而不是让调用方手填, 避免"上报有 browser 但
	// runtime 其实没有"这种对不上的情况。
	caps := MergeCaps(CapsFromRuntime(opt.Runtime.Capabilities()), opt.ExtraCaps, []string{WorkerCap(opt.Name)})
	return &Worker{
		opt:  opt,
		cli:  newCtlClient(opt.Control, opt.Name, opt.EventsPath, opt.AuthToken, opt.HTTPClient),
		caps: caps, kinds: kinds,
	}, nil
}

// Caps 本 worker 上报的能力标签。
func (w *Worker) Caps() []string { return append([]string(nil), w.caps...) }

// Stats 已完成/已失败任务数 (观测)。
func (w *Worker) Stats() (done, failed int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.done, w.fail
}

// Run 阻塞运行: 心跳 goroutine + 拉取-执行循环。ctx 取消后等在途任务收尾再返回。
func (w *Worker) Run(ctx context.Context) error {
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		w.heartbeatLoop(ctx)
	}()

	sem := make(chan struct{}, w.opt.MaxParallel)
	var wg sync.WaitGroup
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case sem <- struct{}{}:
		}

		task, err := w.cli.pull(w.kinds, w.caps)
		if err != nil {
			<-sem
			w.opt.Logf("[worker] 拉取失败: %v", err)
			if !sleepCtx(ctx, w.opt.PollInterval) {
				break loop
			}
			continue
		}
		if task == nil {
			<-sem
			if !sleepCtx(ctx, w.opt.PollInterval) {
				break loop
			}
			continue
		}
		wg.Add(1)
		go func(t *cluster.Task) {
			defer wg.Done()
			defer func() { <-sem }()
			w.execute(ctx, t)
		}(task)
	}
	// 在途任务的 ctx 已随之取消 → 它们会走"关停"分支上报失败, 不留悬挂租约。
	wg.Wait()
	hbWG.Wait()
	return ctx.Err()
}

// RunOnce 拉取并执行至多一个任务 (测试与单发模式)。返回是否真的执行了任务。
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	if err := w.cli.heartbeat(w.caps, w.kinds); err != nil {
		return false, err
	}
	task, err := w.cli.pull(w.kinds, w.caps)
	if err != nil {
		return false, err
	}
	if task == nil {
		return false, nil
	}
	w.execute(ctx, task)
	return true, nil
}

func (w *Worker) heartbeatLoop(ctx context.Context) {
	// 立即注册一次: 控制面 Sync 要先看到 worker 才会把 runtime 注册进 RuntimeRegistry。
	if err := w.cli.heartbeat(w.caps, w.kinds); err != nil {
		w.opt.Logf("[worker] 首次注册失败: %v", err)
	}
	t := time.NewTicker(w.opt.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.cli.heartbeat(w.caps, w.kinds); err != nil {
				// 心跳失败不退出: 控制面短暂不可用时继续重试; 租约到期后控制面
				// 会把本 worker 从 RuntimeRegistry 剔除, 不会有任务再派进来。
				w.opt.Logf("[worker] 心跳失败: %v", err)
			}
		}
	}
}

// execute 执行一个任务并回报终态。
func (w *Worker) execute(parent context.Context, task *cluster.Task) {
	start := time.Now()
	st, err := DecodeStageTask(task.Payload)
	if err != nil {
		w.reportFail(task.ID, err.Error())
		return
	}
	if miss := MissingCaps(task.RequireCaps, w.caps); len(miss) > 0 {
		w.reportFail(task.ID, fmt.Sprintf("worker %s 缺少必需能力 %v (本 worker caps=%v)", w.opt.Name, miss, w.caps))
		return
	}
	if err := w.checkWorkspace(st.Workspace); err != nil {
		w.reportFail(task.ID, err.Error())
		return
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// 节点声明回填 ctx: 生产 factory 在**创建时**从 ctx 读 tool_profile/max_turns。
	if h := st.Hints(); h != (agent.NodeExecHints{}) {
		ctx = agent.WithNodeExecHints(ctx, h)
	}
	// trace 四元组跨进程续上: 远程执行的 LLM 调用记账 (llm.jsonl) 才能与控制面的
	// run/node 对齐, 否则远程节点的 token 账是孤儿。
	if st.RunID != "" || st.NodeID != "" {
		ctx = trace.With(ctx, trace.IDs{RunID: st.RunID, NodeID: st.NodeID})
	}

	ch, execErr := w.opt.Runtime.Execute(ctx, st.ToRuntimeTask())
	if execErr != nil {
		w.reportFail(task.ID, fmt.Sprintf("执行体启动失败: %v", execErr))
		return
	}

	rep := &reporter{cli: w.cli, taskID: task.ID, logf: w.opt.Logf}
	var out, failMsg string
	var sawTerminal bool
	ka := time.NewTicker(w.opt.KeepaliveInterval)
	defer ka.Stop()

drain:
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				break drain
			}
			switch ev.Kind {
			case agent.NodeEventOutput:
				out += ev.Delta
			case agent.NodeEventDone:
				sawTerminal = true
				if ev.Output != "" {
					out = ev.Output
				}
			case agent.NodeEventFailed:
				sawTerminal = true
				failMsg = ev.Err
			}
			rep.add(ev)
			if rep.len() >= DefaultEventBatchMaxLen {
				rep.flush(false)
			}
		case <-ka.C:
			// 一次 keepalive 干三件事: 冲刷事件、续租 (长任务防被队列回收)、
			// 取回控制面的取消指令 (跨进程 Cancel 的投递通道)。
			if rep.flush(false) {
				w.opt.Logf("[worker] 任务 %s 收到取消指令", task.ID)
				cancel()
			}
			if err := w.cli.extend(task.ID); err != nil {
				w.opt.Logf("[worker] 任务 %s 续租失败: %v", task.ID, err)
			}
		}
	}
	rep.flush(true)

	switch {
	case failMsg != "":
		w.reportFail(task.ID, failMsg)
	case !sawTerminal:
		// 通道关了却没有终态事件: 执行体有 bug 或被强杀。不许当成功。
		reason := "执行体未回报终态 (无 done/failed 事件)"
		if ctxErr := ctx.Err(); ctxErr != nil {
			reason = fmt.Sprintf("worker 关停或任务被取消: %v", ctxErr)
		}
		w.reportFail(task.ID, reason)
	default:
		res := NewStageResult(w.opt.Name, out, time.Since(start).Milliseconds())
		if err := w.cli.complete(task.ID, res); err != nil {
			// 回报失败 = 控制面不知道我们成功了。任务会停在 leased 直到租约过期
			// 被判失败 (fail-closed): 图层随后重跑该节点。
			w.opt.Logf("[worker] 任务 %s 回报成功失败: %v", task.ID, err)
			w.bump(false)
			return
		}
		w.bump(true)
	}
}

func (w *Worker) reportFail(taskID, msg string) {
	if err := w.cli.fail(taskID, msg); err != nil {
		w.opt.Logf("[worker] 任务 %s 回报失败失败: %v (原因: %s)", taskID, err, msg)
	}
	w.bump(false)
}

func (w *Worker) bump(ok bool) {
	w.mu.Lock()
	if ok {
		w.done++
	} else {
		w.fail++
	}
	w.mu.Unlock()
}

// checkWorkspace 工作区可提供性检查。
//
// 为什么必须 fail-closed: 产码类节点的产出是文件, 编译门禁在 <cwd>/go.mod 上跑。
// 若任务要求 /srv/teams/foo 而本 worker 只有 /home/x, 静默在 /home/x 里跑会得到
// "阶段成功但下一阶段找不到上一阶段的代码"——这类故障极难归因。
// design/02 §3.3 的 cwd 三档位 (local/pvc/git) 尚未实现, 所以这里只做"能不能提供"
// 的判定: 一致才跑, 不一致就诚实失败。
func (w *Worker) checkWorkspace(want string) error {
	want = strings.TrimSpace(want)
	if want == "" {
		return nil // 未指定: worker 自行决定 (本地 runtime 用宿主 cwd)
	}
	have := strings.TrimSpace(w.opt.Workspace)
	if have == "" {
		return fmt.Errorf("任务要求工作区 %s, 但 worker %s 未声明工作区 (--workspace); 拒绝在错误目录执行", want, w.opt.Name)
	}
	if !sameDir(want, have) {
		return fmt.Errorf("任务要求工作区 %s, 但 worker %s 的工作区是 %s; 拒绝在错误目录执行", want, w.opt.Name, have)
	}
	if fi, err := os.Stat(have); err != nil || !fi.IsDir() {
		return fmt.Errorf("worker %s 声明的工作区 %s 不可用: %v", w.opt.Name, have, err)
	}
	return nil
}

func sameDir(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// reporter —— 事件批量上报
// ─────────────────────────────────────────────────────────────────────────────

type reporter struct {
	cli    *ctlClient
	taskID string
	logf   func(string, ...any)

	mu  sync.Mutex
	buf []agent.NodeEvent
}

func (r *reporter) add(ev agent.NodeEvent) {
	r.mu.Lock()
	r.buf = append(r.buf, ev)
	r.mu.Unlock()
}

func (r *reporter) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buf)
}

// flush 上报缓冲里的事件, 返回控制面是否要求取消。
//
// last=true (收尾) 且缓冲为空时直接返回 —— 任务已结束, 再问一次取消没有意义。
// last=false 且缓冲为空时**仍然发送**: 那正是 keepalive, 它的目的就是取回取消指令。
func (r *reporter) flush(last bool) bool {
	r.mu.Lock()
	batch := r.buf
	r.buf = nil
	r.mu.Unlock()
	if len(batch) == 0 && last {
		return false
	}
	ack, err := r.cli.events(r.taskID, batch)
	if err != nil {
		// 事件是观测流: 上报失败只记日志, 不影响执行与终态回报 (终态走队列)。
		r.logf("[worker] 任务 %s 事件上报失败: %v", r.taskID, err)
		return false
	}
	return ack.Cancel
}

// ─────────────────────────────────────────────────────────────────────────────
// ctlClient —— 控制面 HTTP 客户端 (严格校验状态码)
//
// 为什么不直接用 cluster.Client: 它的 post 拿到状态码却不检查
// (pkg/cluster/http.go:179-182 只在 200 时解码 body, 非 200 一律返回 nil error),
// 于是 "任务不在你的租约内" 这类 400 在 worker 侧是**静默成功**。执行路径上的错误
// 必须暴露, 所以这里自己发请求并按状态码判错。协议类型 (cluster.Task /
// cluster.WorkerInfo) 仍然复用, 不重复定义线格式。
// ─────────────────────────────────────────────────────────────────────────────

type ctlClient struct {
	base       string
	worker     string
	eventsPath string
	token      string
	http       *http.Client
}

func newCtlClient(base, worker, eventsPath, token string, hc *http.Client) *ctlClient {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &ctlClient{base: strings.TrimRight(base, "/"), worker: worker,
		eventsPath: eventsPath, token: strings.TrimSpace(token), http: hc}
}

// post 发送 JSON 并按状态码判错。返回状态码 (204 也算成功)。
func (c *ctlClient) post(path string, body, out any) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, fmt.Errorf("%s 返回 %d: %s", path, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s 应答解码失败: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

func (c *ctlClient) heartbeat(caps, kinds []string) error {
	_, err := c.post("/cluster/heartbeat", cluster.WorkerInfo{Name: c.worker, Caps: caps, Kinds: kinds}, nil)
	return err
}

func (c *ctlClient) pull(kinds, caps []string) (*cluster.Task, error) {
	var out struct {
		Task *cluster.Task `json:"task"`
	}
	code, err := c.post("/cluster/pull", map[string]any{"worker": c.worker, "kinds": kinds, "caps": caps}, &out)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNoContent {
		return nil, nil
	}
	return out.Task, nil
}

func (c *ctlClient) complete(id string, res StageResult) error {
	raw, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return c.withRetry(func() error {
		_, err := c.post("/cluster/complete",
			map[string]any{"id": id, "worker": c.worker, "result": json.RawMessage(raw)}, nil)
		return err
	})
}

func (c *ctlClient) fail(id, msg string) error {
	return c.withRetry(func() error {
		_, err := c.post("/cluster/fail", map[string]any{"id": id, "worker": c.worker, "err": msg}, nil)
		return err
	})
}

func (c *ctlClient) extend(id string) error {
	_, err := c.post("/cluster/extend", map[string]any{"id": id, "worker": c.worker}, nil)
	return err
}

func (c *ctlClient) events(taskID string, evs []agent.NodeEvent) (EventAck, error) {
	var ack EventAck
	_, err := c.post(c.eventsPath, EventBatch{Worker: c.worker, TaskID: taskID, Events: evs}, &ack)
	return ack, err
}

// withRetry 终态回报的有限重试: 丢一次成功结果的代价是整个节点白跑。
// 只重试传输/5xx 类错误——4xx 是语义错误 (如任务已不在租约内), 重试没有意义。
func (c *ctlClient) withRetry(fn func() error) error {
	var last error
	for i := 0; i < DefaultReportRetries; i++ {
		last = fn()
		if last == nil {
			return nil
		}
		if isClientError(last) {
			return last
		}
		time.Sleep(time.Duration(1<<uint(i)) * 200 * time.Millisecond)
	}
	return last
}

func isClientError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, code := range []string{"返回 400", "返回 401", "返回 403", "返回 404", "返回 405", "返回 409"} {
		if strings.Contains(s, code) {
			return true
		}
	}
	return false
}

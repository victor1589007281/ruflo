// harness_smoke_cmd.go — harness 契约层的**真上游**冒烟（k8s Job 与本地共用）。
//
// # 为什么需要这个子命令
//
// pkg/harness 的六项契约（R-H1/R-H2/R-H4/R-H5/R-H11/R-H17）此前只有单测覆盖：
// 假上游证明了"装配正确时行为正确"，但没有任何东西证明**生产装配**（buildEngine
// 解析 providers → api.Client → engine → harness.Runner）这一整条链在真实
// 端点上也成立。实测踩到的两个例子：
//
//   - layout.Build 的断点数大于动态段数，布局被静默丢弃（trace 永远为空）；
//   - 配置里的 promptCacheMode=on 只在 opencode 这类非 Anthropic 域名下才发
//     cache_control，单测用假 URL 时 auto/on 的差别根本看不见。
//
// 两类缺陷都只有"把真实配置喂给真实装配、再观察出站字节"才能抓到。
//
// # 为什么不是 HTTP 端点
//
// 被测对象是进程内契约。为一次验收临时开一个公网可调的 run-turn 接口，等于把
// 测试需求变成生产攻击面；Job 跑完即消失，且可以带真实 Secret。
//
// # 判定口径
//
// 每项检查断言的是**出站请求字节 / trace / 结果字段**，不是"函数被调用过"。
// 报告里同时给出 detail（看到的实际值），失败时能直接定位到哪一层没通电。
//
// 输出约定：末尾打印一行 `HARNESS_SMOKE_REPORT_JSON <compact-json>`，供
// deploy/harness-smoke-k8s.sh 从 Pod 日志里提取；进程退出码非 0 表示有检查失败。
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/harness"
	"github.com/anthropic/claude-go/pkg/harness/harnesstest"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/types"
	"github.com/spf13/cobra"
)

// harnessSmokeCmd 是 `claude-go harness-smoke` 的入口。
//
// --model / --config 是根命令的持久旗标（见 main.go），这里不重复声明 ——
// cobra 同名旗标会 panic，而且 --config 的加载逻辑全在 buildEngine 里。
func harnessSmokeCmd() *cobra.Command {
	var outPath string
	var turnTimeout time.Duration

	cmd := &cobra.Command{
		Use:   "harness-smoke",
		Short: "用真上游跑 harness 契约层冒烟（R-H1/R-H2/R-H4/R-H5/R-H9/R-H11/R-H17）",
		Long: `按生产装配路径（buildEngine）连接 --model 指定的真实模型，逐项验证
harness 契约层的六项能力，并输出机器可读报告。

检查项（任一失败即退出码非 0）：
  build_engine                  providers/model 解析成功
  live_turn_answer              一轮真实对话得到非空回答
  live_usage_cost               上游 usage 被采到，账本成本已知
  live_model_route              出站模型 = 配置里的裸模型名（未带 provider 前缀）
  r_h2_cache_plan_on_wire       trace 里的计划断点与 system 数组上的 cache_control 同源
  r_h1_context_pruned           token 预算裁剪生效，被裁 ID 进 trace，正文不出现在请求里
  r_h4_replay_no_second_call    同 TurnID 重放不重跑模型、不重复落库
  r_h11_running_before_model    模型请求发出时 sink 已有 running 状态
  r_h5_text_attachment          文本附件真的进了请求，转录里存的是引用
  r_h11_image_rejected          deepseek 等纯文本模型在发请求前拒绝图片附件
  r_h9_gate_overrides           确认/幂等闸经 harness → engine 到达工具（本地假上游）

注意：r_h9 用本地假上游，才可能确定性地触发一次工具调用；其余检查走真上游。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			rep := runHarnessSmoke(turnTimeout)
			writeHarnessSmokeReport(rep, outPath)
			if !rep.Pass {
				return fmt.Errorf("harness-smoke: %d/%d 项未通过", rep.failed(), len(rep.Checks))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&outPath, "out", "", "报告写入路径（同时打印 HARNESS_SMOKE_REPORT_JSON 行到 stdout）")
	cmd.Flags().DurationVar(&turnTimeout, "turn-timeout", 240*time.Second, "单轮超时（同时作为 MaxWall 预算）")
	return cmd
}

// ===========================================================================
// 报告
// ===========================================================================

type smokeCheck struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

type smokeReport struct {
	Pass       bool         `json:"pass"`
	Model      string       `json:"model"`
	StartedAt  time.Time    `json:"startedAt"`
	DurationMS int64        `json:"durationMs"`
	Checks     []smokeCheck `json:"checks"`
}

func (r *smokeReport) add(name string, pass bool, detail string) {
	r.Checks = append(r.Checks, smokeCheck{Name: name, Pass: pass, Detail: detail})
}

func (r *smokeReport) failed() int {
	n := 0
	for _, c := range r.Checks {
		if !c.Pass {
			n++
		}
	}
	return n
}

// writeHarnessSmokeReport 同时写文件与 stdout。
//
// stdout 必须是**单行** compact JSON：deploy 脚本用 `sed -n 's/^HARNESS_SMOKE_REPORT_JSON //p'`
// 提取，缩进 JSON 会被截成多行，解析出半截。
func writeHarnessSmokeReport(rep *smokeReport, outPath string) {
	compact, err := json.Marshal(rep)
	if err != nil {
		fmt.Printf("HARNESS_SMOKE_REPORT_JSON {\"pass\":false,\"checks\":[{\"name\":\"marshal\",\"pass\":false,\"detail\":%q}]}\n", err.Error())
		return
	}
	for _, c := range rep.Checks {
		mark := "✅"
		if !c.Pass {
			mark = "❌"
		}
		fmt.Printf("  %s %-28s %s\n", mark, c.Name, c.Detail)
	}
	fmt.Printf("HARNESS_SMOKE_REPORT_JSON %s\n", compact)

	if outPath == "" {
		return
	}
	pretty, _ := json.MarshalIndent(rep, "", "  ")
	dir := filepath.Dir(outPath)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := os.WriteFile(outPath, append(pretty, '\n'), 0o644); err != nil {
		fmt.Printf("  报告写入 %s 失败: %v\n", outPath, err)
	}
}

// ===========================================================================
// 出站请求记录器
// ===========================================================================

type wireRequest struct {
	Path  string
	Model string
	Body  string
}

// wireRecorder 包在 api.Client 的 http.Client.Transport 外层，记录**实际发出的**
// 请求体。断言出站字节是这层唯一的可信来源：只断言 trace 会把"计划算对了但没
// 发出去"判成通过。
type wireRecorder struct {
	base http.RoundTripper

	// onRequest 在请求即将发出时回调（请求体已读完、尚未交给下层）。
	// 用来在"模型调用那一刻"给 sink 状态拍快照 —— 后置检查无法区分
	// "调用前写入 running" 与 "收尾时补写"。
	onRequest func()

	mu   sync.Mutex
	reqs []wireRequest
}

func (w *wireRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)

	w.mu.Lock()
	w.reqs = append(w.reqs, wireRequest{Path: r.URL.Path, Model: probe.Model, Body: string(body)})
	w.mu.Unlock()

	if w.onRequest != nil {
		w.onRequest()
	}
	base := w.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

func (w *wireRecorder) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.reqs)
}

// between 返回 [from, to) 区间的请求副本。
func (w *wireRecorder) between(from, to int) []wireRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	if from < 0 {
		from = 0
	}
	if to > len(w.reqs) {
		to = len(w.reqs)
	}
	if from >= to {
		return nil
	}
	out := make([]wireRequest, to-from)
	copy(out, w.reqs[from:to])
	return out
}

// ===========================================================================
// 内存 sink / trace（只读验收用，不落库）
// ===========================================================================

type memSink struct {
	mu       sync.Mutex
	users    map[string]bool
	userRecs []harness.UserRecord
	assists  map[string]int
	states   []harness.TurnState
}

func newMemSink() *memSink {
	return &memSink{users: map[string]bool{}, assists: map[string]int{}}
}

func sinkKey(owner, session, turn string) string {
	return owner + "\x00" + session + "\x00" + turn
}

func (s *memSink) EnsureUser(_ context.Context, rec harness.UserRecord) (bool, error) {
	k := sinkKey(rec.OwnerID, rec.SessionID, rec.TurnID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.users[k] {
		return true, nil
	}
	s.users[k] = true
	s.userRecs = append(s.userRecs, rec)
	return false, nil
}

func (s *memSink) AppendAssistant(_ context.Context, rec harness.AssistantRecord) error {
	k := sinkKey(rec.OwnerID, rec.SessionID, rec.TurnID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.assists[k]++
	return nil
}

func (s *memSink) MarkTurn(_ context.Context, st harness.TurnState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = append(s.states, st)
	return nil
}

func (s *memSink) stateSnapshot() []harness.TurnState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]harness.TurnState, len(s.states))
	copy(out, s.states)
	return out
}

func (s *memSink) assistantCount(owner, session, turn string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.assists[sinkKey(owner, session, turn)]
}

func (s *memSink) userRecord(owner, session, turn string) (harness.UserRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.userRecs {
		if rec.OwnerID == owner && rec.SessionID == session && rec.TurnID == turn {
			return rec, true
		}
	}
	return harness.UserRecord{}, false
}

type memTrace struct {
	mu     sync.Mutex
	byTurn map[string]harness.TurnTrace
}

func newMemTrace() *memTrace {
	return &memTrace{byTurn: map[string]harness.TurnTrace{}}
}

func (m *memTrace) RecordTurnTrace(_ context.Context, tr harness.TurnTrace) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byTurn[tr.TurnID] = tr
	return nil
}

func (m *memTrace) get(turnID string) (harness.TurnTrace, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr, ok := m.byTurn[turnID]
	return tr, ok
}

// ===========================================================================
// 可切换的上下文提供器
// ===========================================================================

type switchProvider struct {
	mu    sync.Mutex
	patch harness.ContextPatch
}

func (p *switchProvider) set(patch harness.ContextPatch) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.patch = patch
}

func (p *switchProvider) ProvideTurnContext(_ context.Context, _ harness.TurnContext) (harness.ContextPatch, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.patch, nil
}

// ===========================================================================
// 主流程
// ===========================================================================

func runHarnessSmoke(timeout time.Duration) *smokeReport {
	if timeout <= 0 {
		timeout = 240 * time.Second
	}
	start := time.Now()
	rep := &smokeReport{Model: flagModel, StartedAt: start}
	defer func() {
		rep.DurationMS = time.Since(start).Milliseconds()
		rep.Pass = len(rep.Checks) > 0 && rep.failed() == 0
	}()

	eng, err := buildEngine()
	if err != nil {
		rep.add("build_engine", false, "buildEngine: "+err.Error())
		return rep
	}
	if eng.APIClient == nil {
		rep.add("build_engine", false, "buildEngine 返回的 engine 没有 APIClient")
		return rep
	}
	rep.add("build_engine", true, fmt.Sprintf("provider/client 装配完成（model=%s）", flagModel))

	// 让稳定段至少有两段，才能区分"计划打的断点（稳定段末尾）"与 api 内置
	// 启发式（固定 block 0）。这是宿主合法的 AppendPrompt 配置，引擎组装
	// systemPrompt 时走的是同一个 BuildEffectiveSystemPrompt。
	if eng.PromptMgr != nil && eng.PromptMgr.AppendPrompt == "" {
		eng.PromptMgr.AppendPrompt = "附加约束：回答保持简短，不要调用工具。"
	}

	// 记录出站请求；onRequest 在请求发出时给 sink 拍快照（Q-H11）。
	rec := &wireRecorder{}
	if eng.APIClient.Client == nil {
		eng.APIClient.Client = &http.Client{Timeout: 5 * time.Minute}
	}
	rec.base = eng.APIClient.Client.Transport
	eng.APIClient.Client.Transport = rec

	sink := newMemSink()
	tr := newMemTrace()
	provider := &switchProvider{}
	ledger := harness.NewCostLedger(func(_ string, u types.Usage) float64 {
		total := u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		// 价目函数只需返回正数来证明"账本按 usage 计了价"；金额本身不是本检查的对象。
		return float64(total) * 0.000001
	})

	var (
		snapMu        sync.Mutex
		requestStates [][]harness.TurnState
	)
	rec.onRequest = func() {
		snap := sink.stateSnapshot()
		snapMu.Lock()
		requestStates = append(requestStates, snap)
		snapMu.Unlock()
	}

	blobDir, err := os.MkdirTemp("", "harness-smoke-blobs-*")
	if err != nil {
		rep.add("blob_store", false, err.Error())
		return rep
	}
	defer os.RemoveAll(blobDir)
	blobs, err := harness.NewFSBlobStore(blobDir)
	if err != nil {
		rep.add("blob_store", false, err.Error())
		return rep
	}
	rep.add("blob_store", true, blobDir)

	runner := harness.NewRunner(harness.RunnerOptions{
		Engine:             eng,
		Sink:               sink,
		Blobs:              blobs,
		MaxAttachmentBytes: 8 << 20,
		ContextTokenBudget: 100,
		Ledger:             ledger,
		Trace:              tr,
		Layout:             harness.NewLayoutBuilder(harness.LayoutOptions{}),
		ContextProvider:    provider,
		// 角色 → 模型：让 resolveModel 返回非空，模态判据才生效（R-H11）。
		RoleModels: map[string]string{harness.RoleDialogue: flagModel},
		Now:        time.Now,
	})

	const (
		owner     = "smoke-owner"
		sessionID = "smoke-session"
	)
	noTools := []string{}
	runTurn := func(turnID, input string, atts []harness.AttachmentRef) (*harness.TurnResult, int, []wireRequest, error) {
		before := rec.count()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		res, err := runner.RunTurn(ctx, harness.TurnRequest{
			SessionID:   sessionID,
			TurnID:      turnID,
			OwnerID:     owner,
			Input:       []types.ContentBlock{{Type: types.ContentBlockText, Text: input}},
			Attachments: atts,
			Policy: harness.TurnPolicy{
				// 空集 = 全部拒绝（fail-closed）：冒烟只测 harness，不允许模型
				// 顺手在集群里执行 shell 或写文件。
				AllowedTools: &noTools,
				MaxWall:      timeout,
			},
		}, nil)
		return res, before, rec.between(before, rec.count()), err
	}

	// -----------------------------------------------------------------
	// 1) 真实一轮：回答 + usage + 成本 + 路由 + 缓存计划 + running 状态
	// -----------------------------------------------------------------
	const textTurnID = "smoke-text-1"
	resText, beforeText, textReqs, errText := runTurn(textTurnID, "请只回复四个字：集成通过", nil)
	textCalls := rec.count() - beforeText

	rep.add("live_turn_answer", errText == nil && resText != nil && strings.TrimSpace(resText.Text) != "",
		fmt.Sprintf("text=%q err=%v requests=%d", textText(resText), errText, textCalls))

	if resText != nil {
		ok := resText.UsageKnown && resText.Usage.InputTokens > 0 && resText.Usage.OutputTokens > 0
		rep.add("live_usage_cost", ok,
			fmt.Sprintf("usageKnown=%v in=%d out=%d costKnown=%v cost=%.9f",
				resText.UsageKnown, resText.Usage.InputTokens, resText.Usage.OutputTokens,
				resText.CostKnown, resText.Cost))
		if resText.UsageKnown {
			okCost := resText.CostKnown && resText.Cost > 0
			rep.add("live_cost_known", okCost,
				fmt.Sprintf("costKnown=%v cost=%.9f（账本已装配）", resText.CostKnown, resText.Cost))
		}
	}

	// 出站模型由 ResolvedConfig.OutboundModelName 决定，不能一律要求裸名：
	//   - 直连 provider：裸名（带 provider 前缀会被厂商 400）；
	//   - 经 CLAUDE_GO_LLM_GATEWAY：完整别名，网关用别名选 provider。
	// 部署拓扑两种都合法，所以这里以装配后的 APIClient 端点为判据，而不是
	// 无脑把冒烟里的 flagModel 剥前缀 —— 否则网关拓扑会稳定假红。
	wantOutbound := flagModel
	if i := strings.Index(wantOutbound, ":"); i >= 0 {
		wantOutbound = wantOutbound[i+1:]
	}
	routeMode := "直连 provider 发裸名"
	if gw := modelconfig.GatewayBaseURL(); gw != "" &&
		strings.TrimRight(eng.APIClient.BaseURL, "/") == gw {
		wantOutbound = flagModel
		routeMode = "经 LLM 网关发别名"
	}
	routeDetail := "没有出站请求"
	routeOK := false
	if len(textReqs) > 0 {
		sent := textReqs[0].Model
		routeOK = sent == wantOutbound
		routeDetail = fmt.Sprintf("出站 model=%q want=%q path=%s（%s）",
			sent, wantOutbound, textReqs[0].Path, routeMode)
	}
	// 路由可观测：TurnResult.Model 必须是本轮实际使用的身份（这里等于 flagModel）。
	if resText != nil && resText.Model != flagModel {
		routeOK = false
		routeDetail += fmt.Sprintf("；TurnResult.Model=%q want=%q", resText.Model, flagModel)
	}
	rep.add("live_model_route", routeOK, routeDetail)

	// R-H2：trace 里的计划断点必须与出站 system 数组同源。
	cacheOK, cacheDetail := false, "没有 trace"
	if t, ok := tr.get(textTurnID); ok && t.PromptLayout != nil {
		planned := t.PromptLayout.PlannedSystemBreakpoints
		if len(planned) == 0 {
			cacheDetail = "layout 有但 PlannedSystemBreakpoints 为空"
		} else if len(textReqs) == 0 {
			cacheDetail = "计划存在但没有出站请求"
		} else {
			sys := systemParam([]byte(textReqs[0].Body))
			cacheOK = len(sys) >= 2
			if !cacheOK {
				cacheDetail = fmt.Sprintf("system 段数=%d，<2（分不出计划与内置启发式）", len(sys))
			}
			for _, idx := range planned {
				if idx < 0 || idx >= len(sys) {
					cacheOK = false
					cacheDetail = fmt.Sprintf("计划下标 %d 越界（system len=%d）", idx, len(sys))
					continue
				}
				if _, ok := sys[idx]["cache_control"]; !ok {
					cacheOK = false
					cacheDetail = fmt.Sprintf("system[%d] 上没有 cache_control", idx)
				}
			}
			if cacheOK {
				// 反向判据：内置启发式固定打 block 0。计划打的是稳定段末尾
				// （block 1），所以 block 0 必须**没有**断点 —— 否则这个方法
				// 分不出"计划生效"与"计划被无视后内置兜底"。
				if _, ok := sys[0]["cache_control"]; ok {
					cacheOK = false
					cacheDetail = "system[0] 被打点：内置启发式仍在生效，计划被绕过"
				} else {
					cacheDetail = fmt.Sprintf("planned=%v systemSegments=%d 全部命中且 block0 无断点", planned, len(sys))
				}
			}
		}
	} else if ok && t.PromptLayout == nil {
		cacheDetail = "trace 里没有 PromptLayout（布局被丢弃）"
	}
	rep.add("r_h2_cache_plan_on_wire", cacheOK, cacheDetail)

	// Q-H11：第一次请求发出时，sink 里必须有 running 且没有 completed。
	snapMu.Lock()
	var firstSnap []harness.TurnState
	if len(requestStates) > 0 {
		firstSnap = append([]harness.TurnState(nil), requestStates[0]...)
	}
	snapMu.Unlock()
	runningSeen, completedSeen := false, false
	for _, st := range firstSnap {
		if st.TurnID != textTurnID {
			continue
		}
		if st.State == "running" {
			runningSeen = true
		}
		if st.State == "completed" || st.State == "failed" || st.State == "interrupted" {
			completedSeen = true
		}
	}
	rep.add("r_h11_running_before_model", runningSeen && !completedSeen,
		fmt.Sprintf("请求发出时 states=%d running=%v terminalAlready=%v", len(firstSnap), runningSeen, completedSeen))

	// -----------------------------------------------------------------
	// 2) R-H1：token 预算裁剪 + trace + 请求正文
	// -----------------------------------------------------------------
	const ctxTurnID = "smoke-context-1"
	provider.set(harness.ContextPatch{
		Blocks: []harness.ContextBlock{
			{ID: "drop", Text: "CTX_DROP_4Q 低优先级证据", Priority: 1, Tokens: 60},
			{ID: "keep", Text: "CTX_KEEP_9Z 高优先级证据", Priority: 9, Tokens: 60},
		},
		Trace: "retrieval:2",
	})
	resCtx, _, ctxReqs, errCtx := runTurn(ctxTurnID, "请只回复两个字：收到", nil)
	provider.set(harness.ContextPatch{})

	ctxOK := false
	ctxDetail := "没有 trace"
	if errCtx != nil {
		ctxDetail = "本轮失败: " + errCtx.Error()
	} else if t, ok := tr.get(ctxTurnID); ok {
		body := ""
		if len(ctxReqs) > 0 {
			body = ctxReqs[0].Body
		}
		ctxOK = containsStr(t.ContextBlockIDs, "keep") &&
			containsStr(t.ContextDroppedIDs, "drop") &&
			t.ContextTokens == 60 && t.ContextTokensKnown &&
			t.ContextTrace == "retrieval:2" &&
			strings.Contains(body, "CTX_KEEP_9Z") && !strings.Contains(body, "CTX_DROP_4Q") &&
			resCtx != nil && containsStr(resCtx.Degraded, "context_budget_pruned")
		ctxDetail = fmt.Sprintf("kept=%v dropped=%v tokens=%d known=%v trace=%q bodyKeep=%v bodyDrop=%v degraded=%v",
			t.ContextBlockIDs, t.ContextDroppedIDs, t.ContextTokens, t.ContextTokensKnown, t.ContextTrace,
			strings.Contains(body, "CTX_KEEP_9Z"), strings.Contains(body, "CTX_DROP_4Q"), degradedOf(resCtx))
	}
	rep.add("r_h1_context_pruned", ctxOK, ctxDetail)

	// -----------------------------------------------------------------
	// 3) R-H4：同 TurnID 重放不重跑模型、不重复落库
	// -----------------------------------------------------------------
	beforeReplay := rec.count()
	resReplay, _, _, errReplay := runTurn(textTurnID, "请只回复四个字：集成通过", nil)
	replayCalls := rec.count() - beforeReplay
	replayOK := errReplay == nil && replayCalls == 0 && resReplay != nil &&
		resText != nil && resReplay.Text == resText.Text &&
		resReplay.Persisted && containsStr(resReplay.Degraded, "turn_replayed") &&
		sink.assistantCount(owner, sessionID, textTurnID) == 1
	replayDetail := fmt.Sprintf("secondCalls=%d textEqual=%v persisted=%v degraded=%v assists=%d",
		replayCalls, resReplay != nil && resText != nil && resReplay.Text == resText.Text,
		resReplay != nil && resReplay.Persisted, degradedOf(resReplay),
		sink.assistantCount(owner, sessionID, textTurnID))
	if errReplay != nil {
		replayDetail += " err=" + errReplay.Error()
	}
	rep.add("r_h4_replay_no_second_call", replayOK, replayDetail)

	// -----------------------------------------------------------------
	// 4) R-H5：文本附件进请求，转录存引用
	// -----------------------------------------------------------------
	const attachNonce = "ATTACH_NONCE_7K2"
	ref, errAttach := harness.Attach(blobs, harness.AttachmentText, "text/plain", "note.txt",
		[]byte(attachNonce+" 这是随消息上传的文本附件。"))
	attachOK, attachDetail := false, "附件写入 blob 失败: "+errString(errAttach)
	if errAttach == nil {
		const attTurnID = "smoke-attach-1"
		resAtt, _, attReqs, errTurn := runTurn(attTurnID, "附件里有一个代号，请原样回复它。", []harness.AttachmentRef{ref})
		body := ""
		if len(attReqs) > 0 {
			body = attReqs[0].Body
		}
		urec, hasUser := sink.userRecord(owner, sessionID, attTurnID)
		userJSON := ""
		if hasUser {
			b, _ := json.Marshal(urec)
			userJSON = string(b)
		}
		attachOK = errTurn == nil &&
			strings.Contains(body, attachNonce) &&
			hasUser && len(urec.Attachments) == 1 &&
			!strings.Contains(userJSON, attachNonce)
		attachDetail = fmt.Sprintf("err=%v nonceInRequest=%v userAttachments=%d nonceInTranscript=%v text=%q",
			errTurn, strings.Contains(body, attachNonce), len(urec.Attachments),
			strings.Contains(userJSON, attachNonce), textText(resAtt))
	}
	rep.add("r_h5_text_attachment", attachOK, attachDetail)

	// -----------------------------------------------------------------
	// 5) R-H11：纯文本模型在发请求前拒绝图片附件
	// -----------------------------------------------------------------
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9ZJ8sAAAAASUVORK5CYII=")
	imgRef, errImg := harness.Attach(blobs, harness.AttachmentImage, "image/png", "dot.png", png)
	imgOK, imgDetail := false, "图片写入 blob 失败: "+errString(errImg)
	if errImg == nil {
		beforeImg := rec.count()
		const imgTurnID = "smoke-image-1"
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		_, errImgTurn := runner.RunTurn(ctx, harness.TurnRequest{
			SessionID: sessionID, TurnID: imgTurnID, OwnerID: owner,
			Input:       []types.ContentBlock{{Type: types.ContentBlockText, Text: "看图"}},
			Attachments: []harness.AttachmentRef{imgRef},
			Policy:      harness.TurnPolicy{AllowedTools: &noTools, MaxWall: timeout},
		}, nil)
		cancel()
		sent := rec.count() - beforeImg
		var herr *harness.Error
		kind := ""
		if errors.As(errImgTurn, &herr) {
			kind = string(herr.Kind)
		}
		imgOK = kind == string(harness.ErrKindModality) && sent == 0
		imgDetail = fmt.Sprintf("errKind=%q requests=%d err=%v", kind, sent, errImgTurn)
	}
	rep.add("r_h11_image_rejected", imgOK, imgDetail)

	// -----------------------------------------------------------------
	// 6) R-H9：确认/幂等闸经 harness → engine 到达工具
	// -----------------------------------------------------------------
	gateOK, gateDetail := runHarnessGateCheck()
	rep.add("r_h9_gate_overrides", gateOK, gateDetail)

	// -----------------------------------------------------------------
	// 7) H-F1/H-F2/H-F5：完整工具结果落库、信任标记、输入预览
	// -----------------------------------------------------------------
	evOK, evDetail := runHarnessToolEvidenceCheck()
	rep.add("hf1_tool_result_evidence", evOK, evDetail)

	// H-F4：DeepResearch 必须真实出现在需要检索的档位里。这里只验"注册"，
	// 联网研究本身由单测（fake runner）与真实使用验收，不在冒烟里烧配额。
	researchNames := builtin.ToolNameCatalog(builtin.ToolProfileResearch)
	analysisNames := builtin.ToolNameCatalog(builtin.ToolProfileAnalysis)
	deepOK := containsStr(researchNames, builtin.DeepResearchToolName) &&
		containsStr(analysisNames, builtin.DeepResearchToolName)
	rep.add("hf4_deepresearch_profile", deepOK,
		fmt.Sprintf("research=%v analysis=%v", containsStr(researchNames, builtin.DeepResearchToolName),
			containsStr(analysisNames, builtin.DeepResearchToolName)))

	return rep
}

// ===========================================================================
// R-H9：用本地假上游确定性地触发一次工具调用
// ===========================================================================

// gateProbe 记录工具眼里看到的闸。工具批次可能并发，全部加锁。
type gateProbe struct {
	mu         sync.Mutex
	calls      int
	confirmed  bool
	idempotent bool
}

func (p *gateProbe) Call(_ context.Context, _ json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.confirmed = tctx != nil && tctx.Confirmed != nil
	p.idempotent = tctx != nil && tctx.Idempotency != nil
	return &tool.ToolResult{Content: "probe ok"}, nil
}

func (p *gateProbe) snapshot() (int, bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.confirmed, p.idempotent
}

// gateProbeTool 刻意不实现 NeedsConfirmation：若声明了，orchestration 会在
// Confirmed == nil 时直接拒绝，Call 根本不跑 —— "没接线"与"接线了"就无法通过
// Call 自身分辨。这里要钉的是构造点有没有把闸交给工具。
type gateProbeTool struct{ p *gateProbe }

func (t *gateProbeTool) Name() string        { return "smoke_gate_probe" }
func (t *gateProbeTool) Description() string { return "R-H9 接线探针" }
func (t *gateProbeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t *gateProbeTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }
func (t *gateProbeTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}
func (t *gateProbeTool) IsReadOnly(_ json.RawMessage) bool { return true }
func (t *gateProbeTool) Call(ctx context.Context, in json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	return t.p.Call(ctx, in, tctx)
}

// alwaysConfirmed 是宿主确认的最小实现：一律已确认。
type alwaysConfirmed struct{}

func (alwaysConfirmed) IsConfirmed(string, json.RawMessage) bool { return true }

func runHarnessGateCheck() (bool, string) {
	fm := harnesstest.New(
		harnesstest.ToolCall("smoke_gate_probe", "tu_smoke", "{}"),
		harnesstest.Text("工具已执行"),
	)
	srv := httptest.NewServer(fm.Handler())
	defer srv.Close()

	reg := tool.NewRegistry()
	probe := &gateProbe{}
	reg.Register(&gateProbeTool{p: probe})

	client := &api.Client{
		BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client(), RetryCount: 0,
	}
	promptDir, err := os.MkdirTemp("", "harness-smoke-prompt-*")
	if err != nil {
		return false, "临时目录创建失败: " + err.Error()
	}
	defer os.RemoveAll(promptDir)
	promptMgr := prompt.NewManager(promptDir)
	promptMgr.OverridePrompt = "你是执行者"
	eng := engine.NewQueryEngine(&engine.Config{Model: "m", MaxTurns: 4},
		client, reg, hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeAcceptEdits), nil, promptMgr)

	r := harness.NewRunner(harness.RunnerOptions{
		Engine:       eng,
		Confirmation: alwaysConfirmed{},
		Idempotency:  tool.NewIdempotencyGate(true, 0),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err = r.RunTurn(ctx, harness.TurnRequest{
		SessionID: "gate-session",
		TurnID:    "gate-turn",
		Input:     []types.ContentBlock{{Type: types.ContentBlockText, Text: "调用探针工具"}},
	}, nil)

	calls, confirmed, idem := probe.snapshot()
	ok := err == nil && calls == 1 && confirmed && idem && fm.CallCount() >= 2
	return ok, fmt.Sprintf("toolCalls=%d confirmed=%v idempotency=%v modelCalls=%d err=%v",
		calls, confirmed, idem, fm.CallCount(), err)
}

// ===========================================================================
// H-F1/H-F2/H-F5：工具证据闭环的确定性验收
// ===========================================================================

// evidenceProbeTool 返回一段超过 500B 的结果，并声明自己来自不可信网页。
// 它同时钉住三件事：全文能到达 ToolResultSink、Trust/SourceURI 不被丢掉、
// 预览不超过 500B。
type evidenceProbeTool struct{ full string }

func (t *evidenceProbeTool) Name() string        { return "smoke_evidence_probe" }
func (t *evidenceProbeTool) Description() string { return "H-F1 证据闭环探针" }
func (t *evidenceProbeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
}
func (t *evidenceProbeTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }
func (t *evidenceProbeTool) IsReadOnly(_ json.RawMessage) bool        { return true }
func (t *evidenceProbeTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}
func (t *evidenceProbeTool) Call(_ context.Context, _ json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	return &tool.ToolResult{
		Content:   t.full,
		Trust:     "untrusted",
		SourceURI: "https://example.com/evidence",
	}, nil
}

type evidenceSink struct {
	mu   sync.Mutex
	recs []harness.ToolResultRecord
}

func (s *evidenceSink) AppendToolResult(_ context.Context, rec harness.ToolResultRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, rec)
	return nil
}

func (s *evidenceSink) snapshot() []harness.ToolResultRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]harness.ToolResultRecord, len(s.recs))
	copy(out, s.recs)
	return out
}

// runHarnessToolEvidenceCheck 用假上游确定性地触发一次工具调用，再从
// ToolResultSink / TurnResult / 流式事件三个出口交叉核对。
//
// 为什么不用真模型：让真模型"恰好调用探针"不可靠，失败时也无法区分是
// 证据链没通还是模型没选工具。这里模型的唯一职责是回放脚本。
func runHarnessToolEvidenceCheck() (bool, string) {
	// 671B：超过 500B 预览窗口，才能同时断言 ResultTruncated 与阈值。
	full := "EVIDENCE_HEAD_" + strings.Repeat("0123456789abcdef", 40) + "EVIDENCE_TAIL_END"

	fm := harnesstest.New(
		harnesstest.ToolCall("smoke_evidence_probe", "tu_evidence", `{"q":"probe"}`),
		harnesstest.Text("证据已记录"),
	)
	srv := httptest.NewServer(fm.Handler())
	defer srv.Close()

	reg := tool.NewRegistry()
	reg.Register(&evidenceProbeTool{full: full})

	client := &api.Client{
		BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client(), RetryCount: 0,
	}
	promptDir, err := os.MkdirTemp("", "harness-smoke-prompt-*")
	if err != nil {
		return false, "临时目录创建失败: " + err.Error()
	}
	defer os.RemoveAll(promptDir)
	promptMgr := prompt.NewManager(promptDir)
	promptMgr.OverridePrompt = "你是执行者"
	eng := engine.NewQueryEngine(&engine.Config{Model: "m", MaxTurns: 4},
		client, reg, hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeAcceptEdits), nil, promptMgr)

	sink := &evidenceSink{}
	var (
		evMu   sync.Mutex
		events []harness.TurnEvent
	)
	r := harness.NewRunner(harness.RunnerOptions{
		Engine:      eng,
		ToolResults: sink,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := r.RunTurn(ctx, harness.TurnRequest{
		SessionID: "evidence-session",
		TurnID:    "evidence-turn",
		OwnerID:   "evidence-owner",
		Input:     []types.ContentBlock{{Type: types.ContentBlockText, Text: "调用证据探针"}},
	}, func(ev harness.TurnEvent) {
		evMu.Lock()
		events = append(events, ev)
		evMu.Unlock()
	})

	recs := sink.snapshot()
	var startTool, doneTool *harness.ToolEvent
	evMu.Lock()
	for _, ev := range events {
		if ev.Tool == nil {
			continue
		}
		switch ev.Kind {
		case harness.EventToolStart:
			startTool = ev.Tool
		case harness.EventToolDone:
			doneTool = ev.Tool
		}
	}
	evMu.Unlock()

	recOK := len(recs) == 1 &&
		recs[0].FullResult == full &&
		recs[0].Trust == "untrusted" &&
		recs[0].SourceURI == "https://example.com/evidence" &&
		recs[0].OwnerID == "evidence-owner" &&
		recs[0].SessionID == "evidence-session" &&
		recs[0].TurnID == "evidence-turn" &&
		strings.Contains(string(recs[0].Input), "probe")

	callOK := res != nil && len(res.ToolCalls) == 1 &&
		res.ToolCalls[0].ResultBytes == len(full) &&
		res.ToolCalls[0].ResultTruncated &&
		len(res.ToolCalls[0].ResultPreview) == 500 &&
		res.ToolCalls[0].Trust == "untrusted" &&
		res.ToolCalls[0].SourceURI == "https://example.com/evidence"

	eventOK := startTool != nil && doneTool != nil &&
		strings.Contains(startTool.InputPreview, "probe") && len(startTool.InputPreview) <= 500 &&
		len(doneTool.ResultPreview) == 500 &&
		doneTool.Trust == "untrusted" &&
		doneTool.SourceURI == "https://example.com/evidence"

	ok := err == nil && recOK && callOK && eventOK
	detail := fmt.Sprintf("err=%v sinkRecords=%d fullMatch=%v trust=%q source=%q callBytes=%d callTrunc=%v previewLen=%d eventTrust=%q startInput=%q",
		err, len(recs), recOK,
		recsField(recs, func(r harness.ToolResultRecord) string { return r.Trust }),
		recsField(recs, func(r harness.ToolResultRecord) string { return r.SourceURI }),
		callResultBytes(res), callResultTruncated(res), callPreviewLen(res),
		toolTrust(doneTool), toolInput(startTool))
	return ok, detail
}

func recsField(recs []harness.ToolResultRecord, f func(harness.ToolResultRecord) string) string {
	if len(recs) == 0 {
		return ""
	}
	return f(recs[0])
}

func callResultBytes(r *harness.TurnResult) int {
	if r == nil || len(r.ToolCalls) == 0 {
		return -1
	}
	return r.ToolCalls[0].ResultBytes
}

func callResultTruncated(r *harness.TurnResult) bool {
	return r != nil && len(r.ToolCalls) > 0 && r.ToolCalls[0].ResultTruncated
}

func callPreviewLen(r *harness.TurnResult) int {
	if r == nil || len(r.ToolCalls) == 0 {
		return -1
	}
	return len(r.ToolCalls[0].ResultPreview)
}

func toolTrust(t *harness.ToolEvent) string {
	if t == nil {
		return ""
	}
	return t.Trust
}

func toolInput(t *harness.ToolEvent) string {
	if t == nil {
		return ""
	}
	return t.InputPreview
}

// ===========================================================================
// 小工具
// ===========================================================================

func textText(r *harness.TurnResult) string {
	if r == nil {
		return ""
	}
	return r.Text
}

func degradedOf(r *harness.TurnResult) []string {
	if r == nil {
		return nil
	}
	return r.Degraded
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func containsStr(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// systemParam 从请求体里取 system 数组。
func systemParam(body []byte) []map[string]any {
	var wire struct {
		System []map[string]any `json:"system"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil
	}
	return wire.System
}

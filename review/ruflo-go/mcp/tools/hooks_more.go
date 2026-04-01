// hooks_more.go：扩展 hooks 与 intelligence 相关 MCP 工具（pre/post edit/command、SONA 轨迹、
// ReasoningBank 模式存储/检索、telemetry 文件 intelligence.json）；与 hooks.go 中 hooksTools 组合使用。

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/embeddings"
	"github.com/ruflo/ruflo-go/pkg/hooks"
	nlp "github.com/ruflo/ruflo-go/pkg/neural"
)

// hooksMoreTools 返回附加的一大批工具定义（多数通过 toolHandler 绑定 map 风格处理函数）。
func hooksMoreTools() []*mcp.MCPTool {
	obj := map[string]any{"type": "object", "properties": map[string]any{}}
	return []*mcp.MCPTool{
		{Name: "hooks_pre-edit", Description: "Pre-edit hook", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"file": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHooksPreEdit)},
		{Name: "hooks_post-edit", Description: "Post-edit hook", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"file": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHooksPostEdit)},
		{Name: "hooks_pre-command", Description: "Pre-command hook", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHooksPreCommand)},
		{Name: "hooks_post-command", Description: "Post-command hook", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHooksPostCommand)},
		{Name: "hooks_list", Description: "List registered hook handlers", InputSchema: obj, Handler: toolHandler(handleHooksList)},
		{Name: "hooks_explain", Description: "Explain hook topic (lightweight)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"topic": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHooksExplain)},
		{Name: "hooks_pretrain", Description: "Pretrain hook placeholder", InputSchema: obj, Handler: toolHandler(handleHooksPretrain)},
		{Name: "hooks_build-agents", Description: "Build-agents hook placeholder", InputSchema: obj, Handler: toolHandler(handleHooksBuildAgents)},
		{Name: "hooks_transfer", Description: "Transfer learning hook", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHooksTransfer)},
		{Name: "hooks_session-restore", Description: "Session restore hook", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"session_id": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHooksSessionRestoreTool)},
		{Name: "hooks_notify", Description: "Notification hook", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHooksNotify)},
		{Name: "hooks_init", Description: "Initialize hooks subsystem record", InputSchema: obj, Handler: toolHandler(handleHooksInit)},
		{Name: "hooks_intelligence", Description: "Intelligence pipeline summary", InputSchema: obj, Handler: toolHandler(handleHooksIntelligence)},
		{Name: "hooks_intelligence-reset", Description: "Reset intelligence telemetry file", InputSchema: obj, Handler: toolHandler(handleHooksIntelligenceReset)},
		{Name: "hooks_intelligence_trajectory-start", Description: "Trajectory start (SONA)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"task_id": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHooksIntelTrajStart)},
		{Name: "hooks_intelligence_trajectory-step", Description: "Trajectory step (SONA)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"trajectory_id": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "required": []string{"trajectory_id"}}, Handler: toolHandler(handleHooksIntelTrajStep)},
		{Name: "hooks_intelligence_trajectory-end", Description: "Trajectory end (SONA)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"trajectory_id": map[string]any{"type": "string"}, "verdict": map[string]any{"type": "string"}}, "required": []string{"trajectory_id"}}, Handler: toolHandler(handleHooksIntelTrajEnd)},
		{Name: "hooks_intelligence_pattern-store", Description: "Store guidance pattern", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"strategy": map[string]any{"type": "string"}, "domain": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHooksIntelPatternStore)},
		{Name: "hooks_intelligence_pattern-search", Description: "Search patterns by embedding", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}, "k": map[string]any{"type": "number"}}}, Handler: toolHandler(handleHooksIntelPatternSearch)},
		{Name: "hooks_intelligence_stats", Description: "SONA + bank stats", InputSchema: obj, Handler: toolHandler(handleHooksIntelStats)},
		{Name: "hooks_intelligence_learn", Description: "Record SONA signal", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"kind": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHooksIntelLearn)},
		{Name: "hooks_intelligence_attention", Description: "Attention weights stub", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"focus": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHooksIntelAttention)},
		{Name: "hooks_worker-detect", Description: "Dispatch detect-class workers", InputSchema: obj, Handler: toolHandler(handleHooksWorkerDetect)},
		{Name: "hooks_model-outcome", Description: "Record model call outcome", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"tokens": map[string]any{"type": "number"}}}, Handler: toolHandler(handleHooksModelOutcome)},
		{Name: "hooks_model-stats", Description: "LLM hook metrics", InputSchema: obj, Handler: toolHandler(handleHooksModelStats)},
		{Name: "hooks_worker-cancel", Description: "Cancel worker job (best-effort stub)", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"worker": map[string]any{"type": "string"}}}, Handler: toolHandler(handleHooksWorkerCancel)},
	}
}

var (
	intelMu     sync.Mutex
	intelEvents []map[string]any
)

func intelPath() string {
	return filepath.Join(resolveDataDir(), "hooks", "intelligence.json")
}

func intelAppend(ev map[string]any) {
	intelMu.Lock()
	intelEvents = append(intelEvents, ev)
	if len(intelEvents) > 500 {
		intelEvents = intelEvents[len(intelEvents)-500:]
	}
	intelMu.Unlock()
	_ = writeJSONFile(intelPath(), map[string]any{"events": intelEvents, "updated": now()})
}

func handleHooksPreEdit(_ context.Context, m map[string]any) mcp.MCPToolResult {
	hc := hooks.HookContext{File: strArg(m, "file"), Args: m}
	res := globalState.hookExec.Execute(hooks.HookEventPreEdit, hc)
	logHook("hooks_pre-edit", m, res)
	return hookRes(res)
}

func handleHooksPostEdit(_ context.Context, m map[string]any) mcp.MCPToolResult {
	hc := hooks.HookContext{File: strArg(m, "file"), Args: m}
	res := globalState.hookExec.Execute(hooks.HookEventPostEdit, hc)
	logHook("hooks_post-edit", m, res)
	return hookRes(res)
}

// handleHooksPreCommand 在命令执行前运行钩子（Command 字段）。
func handleHooksPreCommand(_ context.Context, m map[string]any) mcp.MCPToolResult {
	cmd := strArg(m, "command")
	hc := hooks.HookContext{Command: cmd, Args: m}
	res := globalState.hookExec.Execute(hooks.HookEventPreCommand, hc)
	logHook("hooks_pre-command", m, res)
	return hookRes(res)
}

func handleHooksPostCommand(_ context.Context, m map[string]any) mcp.MCPToolResult {
	cmd := strArg(m, "command")
	hc := hooks.HookContext{Command: cmd, Args: m}
	res := globalState.hookExec.Execute(hooks.HookEventPostCommand, hc)
	logHook("hooks_post-command", m, res)
	return hookRes(res)
}

func handleHooksList(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	regs := globalState.hookReg.List()
	out := make([]map[string]any, 0, len(regs))
	for _, r := range regs {
		out = append(out, map[string]any{"name": r.Name, "event": string(r.Event), "priority": r.Priority})
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"hooks": out, "count": len(out)}}
}

func handleHooksExplain(_ context.Context, m map[string]any) mcp.MCPToolResult {
	topic := strArg(m, "topic")
	if topic == "" {
		topic = "hooks"
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{
		"topic": topic, "summary": "Hook chains run around edits, commands, tasks, and sessions via pkg/hooks executor.",
	}}
}

func handleHooksPretrain(_ context.Context, m map[string]any) mcp.MCPToolResult {
	res := hooks.HookResult{Success: true, Message: "pretrain scheduled", Data: map[string]any{"mode": "noop"}}
	logHook("hooks_pretrain", m, res)
	return hookRes(res)
}

func handleHooksBuildAgents(_ context.Context, m map[string]any) mcp.MCPToolResult {
	res := hooks.HookResult{Success: true, Message: "agent templates refreshed", Data: map[string]any{"mode": "noop"}}
	logHook("hooks_build-agents", m, res)
	return hookRes(res)
}

func handleHooksTransfer(_ context.Context, m map[string]any) mcp.MCPToolResult {
	pat := strArg(m, "pattern")
	p := &hooks.GuidancePattern{Strategy: "transfer", Domain: pat, Quality: 0.5}
	stored, _ := globalState.reasoningBank.StorePattern(p)
	res := hooks.HookResult{Success: true, Data: map[string]any{"pattern_id": stored.ID}}
	logHook("hooks_transfer", m, res)
	return hookRes(res)
}

func handleHooksSessionRestoreTool(_ context.Context, m map[string]any) mcp.MCPToolResult {
	hc := hooks.HookContext{
		Session: map[string]any{"session_id": strArg(m, "session_id"), "phase": "restore"},
		Args:    m,
	}
	res := globalState.hookExec.Execute(hooks.HookEventSessionRestore, hc)
	logHook("hooks_session-restore", m, res)
	return hookRes(res)
}

func handleHooksNotify(_ context.Context, m map[string]any) mcp.MCPToolResult {
	res := hooks.HookResult{Success: true, Message: strArg(m, "message")}
	logHook("hooks_notify", m, res)
	return hookRes(res)
}

func handleHooksInit(_ context.Context, m map[string]any) mcp.MCPToolResult {
	res := hooks.HookResult{Success: true, Message: "hooks ready", Data: map[string]any{"registry_hooks": len(globalState.hookReg.List())}}
	logHook("hooks_init", m, res)
	return hookRes(res)
}

func handleHooksIntelligence(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	intelMu.Lock()
	n := len(intelEvents)
	intelMu.Unlock()
	sonaN := 0
	if globalState.sona != nil {
		sonaN = len(globalState.sona.Patterns())
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"intel_events": n, "sona_patterns": sonaN}}
}

// handleHooksIntelligenceReset 清空内存事件并重写空 intelligence.json。
func handleHooksIntelligenceReset(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	intelMu.Lock()
	intelEvents = nil
	intelMu.Unlock()
	_ = writeJSONFile(intelPath(), map[string]any{"events": []any{}, "updated": now()})
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"reset": true}}
}

// handleHooksIntelTrajStart 调用 SONA BeginTrajectory 并 intelAppend 记录。
func handleHooksIntelTrajStart(_ context.Context, m map[string]any) mcp.MCPToolResult {
	if globalState.sona == nil {
		return mcp.MCPToolResult{OK: false, Error: "sona nil"}
	}
	id := globalState.sona.BeginTrajectory(strArg(m, "task_id"))
	intelAppend(map[string]any{"kind": "traj_start", "id": id, "at": now()})
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"trajectory_id": id}}
}

// handleHooksIntelTrajStep 向轨迹追加 Observation 步骤。
func handleHooksIntelTrajStep(_ context.Context, m map[string]any) mcp.MCPToolResult {
	tid := strArg(m, "trajectory_id")
	if globalState.sona == nil {
		return mcp.MCPToolResult{OK: false, Error: "sona nil"}
	}
	globalState.sona.RecordStep(tid, nlp.TrajectoryStep{
		Type:    nlp.StepObservation,
		Content: strArg(m, "content"),
	})
	intelAppend(map[string]any{"kind": "traj_step", "id": tid, "at": now()})
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"trajectory_id": tid}}
}

// handleHooksIntelTrajEnd 结束轨迹并传入 verdict 字符串。
func handleHooksIntelTrajEnd(_ context.Context, m map[string]any) mcp.MCPToolResult {
	tid := strArg(m, "trajectory_id")
	if globalState.sona == nil {
		return mcp.MCPToolResult{OK: false, Error: "sona nil"}
	}
	_ = globalState.sona.EndTrajectory(tid, strArg(m, "verdict"))
	intelAppend(map[string]any{"kind": "traj_end", "id": tid, "at": now()})
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"trajectory_id": tid}}
}

// handleHooksIntelPatternStore 将 strategy/domain 写入 ReasoningBank。
func handleHooksIntelPatternStore(_ context.Context, m map[string]any) mcp.MCPToolResult {
	p := &hooks.GuidancePattern{
		Strategy: strArg(m, "strategy"),
		Domain:   strArg(m, "domain"),
		Quality:  0.6,
	}
	stored, err := globalState.reasoningBank.StorePattern(p)
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"id": stored.ID}}
}

// handleHooksIntelPatternSearch 对 query 做 HashEmbed384 后在 ReasoningBank 中 Top-K 搜索。
func handleHooksIntelPatternSearch(_ context.Context, m map[string]any) mcp.MCPToolResult {
	q := strArg(m, "query")
	k := 5
	if v, ok := m["k"].(float64); ok {
		k = int(v)
	}
	emb := embeddings.HashEmbed384(q)
	hits := globalState.reasoningBank.SearchPatterns(emb, k)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"patterns": hits}}
}

// handleHooksIntelStats 汇总 hooksLog 条数与 SONA 模式数。
func handleHooksIntelStats(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	globalState.mu.RLock()
	nLog := len(globalState.hooksLog)
	globalState.mu.RUnlock()
	out := map[string]any{"hooks_log_entries": nLog}
	if globalState.sona != nil {
		out["sona_patterns"] = len(globalState.sona.Patterns())
	}
	return mcp.MCPToolResult{OK: true, Data: out}
}

// handleHooksIntelLearn 向 SONA 记录一条 Signal（kind + 全参负载）。
func handleHooksIntelLearn(_ context.Context, m map[string]any) mcp.MCPToolResult {
	if globalState.sona == nil {
		return mcp.MCPToolResult{OK: false, Error: "sona nil"}
	}
	globalState.sona.RecordSignal(nlp.Signal{Kind: strArg(m, "kind"), Payload: m, Timestamp: time.Now().UTC()})
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"recorded": true}}
}

// handleHooksIntelAttention 返回固定权重向量占位（与 focus 回显）。
func handleHooksIntelAttention(_ context.Context, m map[string]any) mcp.MCPToolResult {
	f := strArg(m, "focus")
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"focus": f, "weights": []float64{0.5, 0.3, 0.2}}}
}

// handleHooksWorkerDetect 以 detect 上下文向 audit 触发器派发 Worker。
func handleHooksWorkerDetect(ctx context.Context, m map[string]any) mcp.MCPToolResult {
	wctx := hooks.WorkerContext{Trigger: "detect", Args: m}
	results := globalState.workerMgr.Dispatch(ctx, "audit", wctx)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"results": results}}
}

// handleHooksModelOutcome 构造桩 LLMResponse 并调用 PostLLMCallHook 记录 token 用量。
func handleHooksModelOutcome(_ context.Context, m map[string]any) mcp.MCPToolResult {
	tok := 0
	if v, ok := m["tokens"].(float64); ok {
		tok = int(v)
	}
	resp := &api.LLMResponse{
		Provider: api.LLMProviderAnthropic,
		Model:    "stub",
		Text:     "ok",
		Usage:    api.LLMUsage{TotalTokens: tok},
	}
	globalState.llmHooks.PostLLMCallHook("mcp-hooks_model-outcome", resp, 0, 0)
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"recorded": true}}
}

// handleHooksModelStats 返回 LLMHookBundle.MetricsSnapshot。
func handleHooksModelStats(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"metrics": globalState.llmHooks.MetricsSnapshot()}}
}

// handleHooksWorkerCancel 占位：声明未跟踪可取消任务。
func handleHooksWorkerCancel(_ context.Context, m map[string]any) mcp.MCPToolResult {
	w := strArg(m, "worker")
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"worker": w, "cancelled": false, "note": "no active cancellable job tracked"}}
}

// hookRes 将 hooks.HookResult 转为 MCPToolResult（Success 且 Error 空视为 OK）。
func hookRes(res hooks.HookResult) mcp.MCPToolResult {
	ok := res.Success && res.Error == ""
	return mcp.MCPToolResult{OK: ok, Data: map[string]any{"result": res}, Error: res.Error}
}

func init() {
	// 启动时尝试从 intelligence.json 恢复 intelEvents，供 hooks_intelligence 系列读取。
	b, err := os.ReadFile(intelPath())
	if err != nil {
		return
	}
	var wrap struct {
		Events []map[string]any `json:"events"`
	}
	if json.Unmarshal(b, &wrap) == nil {
		intelMu.Lock()
		intelEvents = wrap.Events
		intelMu.Unlock()
	}
}

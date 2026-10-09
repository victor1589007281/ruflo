package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// TestTaskInstructionSentOnce 锁住"任务指令只发一份"这条契约。
//
// 背景: TaskInstruction 有两个消费点 —— queryLoop 把它追加到 system prompt 末尾,
// SubmitMessage 又曾在 userContent 为空时把它当成首条 user 消息的正文。两者叠加,
// 每次 llm_call 会把整份阶段提示词原样发两遍 (实测 TraceStore 里团队 stage 的 user
// 段与最后一个 system 段逐字节相同: if-question-expand 单请求 76KB 中 29KB 是重复,
// 全量 5645 次 llm_call 100% 命中)。
//
// 断言方式刻意选"数出现次数"而不是比对某一段: 重复是**位置无关**的浪费, 只要同一份
// 正文在请求体里出现两次就是 bug, 出现在 system 还是 messages 里都一样。
func TestTaskInstructionSentOnce(t *testing.T) {
	const marker = "TASK-INSTRUCTION-MARKER-9f3a"

	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		resp := types.APIResponse{
			ID: "msg_1", Type: "message", Role: "assistant", Model: "test-model",
			Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: "ok"}},
			StopReason: "end_turn",
			Usage:      &types.Usage{InputTokens: 1, OutputTokens: 1},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "test-model", Client: srv.Client()}
	e := NewQueryEngine(&Config{MaxTurns: 1}, client, tool.NewRegistry(), nil, nil, nil, prompt.NewManager(t.TempDir()))
	e.TaskInstruction = marker

	// 团队 stage 的真实调用形态: 任务指令已经由调用方放进 TaskInstruction,
	// 提交时 userContent 为空。
	for range e.SubmitMessage(context.Background(), "") {
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("没有捕获到任何请求体")
	}
	body := bodies[0]
	if n := strings.Count(body, marker); n != 1 {
		t.Fatalf("任务指令在请求体中出现 %d 次, 期望 1 次 (system 一份, user 侧不得复制)\nbody=%s",
			n, truncateForTest(body, 600))
	}
	// 首条 user 消息必须仍然存在: 部分 provider 要求消息序列以 user 轮开始。
	var payload struct {
		// system 的实际形态随 provider 而异 (字符串或 content block 数组),
		// 这里只关心"非空", 形态交给 client 自己的契约测试。
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("请求体不是预期 JSON: %v", err)
	}
	if len(payload.Messages) == 0 || payload.Messages[0].Role != "user" {
		t.Fatalf("首条消息应为 user, got %+v", payload.Messages)
	}
	if !contentHasText(payload.Messages[0].Content) {
		t.Fatalf("首条 user 消息内容为空, 应保留一句指向 system 的短提示: %#v", payload.Messages[0].Content)
	}
	if len(payload.System) == 0 || string(payload.System) == "null" {
		t.Fatal("system 段为空, 任务指令无处安放")
	}
}

// contentHasText 兼容裸字符串与 content block 数组两种合法形态。
func contentHasText(content any) bool {
	switch v := content.(type) {
	case string:
		return v != ""
	case []any:
		for _, b := range v {
			if m, ok := b.(map[string]any); ok {
				if s, _ := m["text"].(string); s != "" {
					return true
				}
			}
		}
	}
	return false
}

func truncateForTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(截断)"
}

// TestTaskInstructionPrecedesMemoryBlock 锁住稳定/可变分层:
// 「base(稳定) → task(本轮任务) → memory(检索结果)」。
//
// 为什么这条顺序是硬契约: 前缀缓存断在第一个不同的字节。判据必须落在**相对次序**
// 上, 因为两种错法各有一半:
//
//	· 任务指令排在**所有** hook 块之后 (历史形态): run 内看着没问题, 但跨 run 时
//	  memory 块的漂移会让它后面的一切作废 —— 实测跨 run 首轮只读到 1024 token。
//	· 记忆块排在任务指令之前 (同一个错的另一种写法): 记忆漂 637 字符, 后面 10K
//	  级的任务块跟着作废。
//
// 两种都表现为"缓存命中率莫名低", 只断言"任务指令存在"一个都抓不住。所以这里
// **真的接一个记忆库**, 让记忆块确实被注入 —— 否则注入路径整个不跑, 顺序怎么写
// 都是绿的 (本用例早先就漏在这一点上)。
//
// 同时保留"无记忆可注入时, 任务指令就是最后一块"这条退化情形。
func TestTaskInstructionPrecedesMemoryBlock(t *testing.T) {
	const marker = "TASK-LAST-BLOCK-MARKER-4b7e"

	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		resp := types.APIResponse{
			ID: "msg_1", Type: "message", Role: "assistant", Model: "test-model",
			Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: "ok"}},
			StopReason: "end_turn",
			Usage:      &types.Usage{InputTokens: 1, OutputTokens: 1},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "test-model", Client: srv.Client()}

	// 记忆库: 内容含 marker 里的词, 保证 BM25 能捞回它 —— 记忆块必须**真的**被
	// 注入, 否则这条用例量的是"没有记忆时"的顺序, 抓不到要抓的那个缺陷。
	store := memory.NewTieredStore()
	store.Add(&memory.MemoryEntry{
		Content:    "TASK-LAST-BLOCK-MARKER 相关的团队产出与阶段结论",
		Source:     "team_result",
		Importance: 0.9,
	})

	e := NewQueryEngine(&Config{MaxTurns: 1}, client, tool.NewRegistry(), nil, nil, nil, prompt.NewManager(t.TempDir()))
	e.MemoryStore = store
	e.RefreshHooks() // 组件是构造后赋的, 不补这一次 MemoryInject 会静默缺席
	e.TaskInstruction = marker

	for range e.SubmitMessage(context.Background(), "") {
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("没有捕获到任何请求体")
	}
	var payload struct {
		System []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &payload); err != nil {
		t.Fatalf("请求体不是预期 JSON: %v", err)
	}
	if len(payload.System) == 0 {
		t.Fatal("system 段为空")
	}

	taskAt, memAt := -1, -1
	for i, blk := range payload.System {
		if strings.Contains(blk.Text, marker) {
			taskAt = i
		}
		if strings.Contains(blk.Text, "<relevant_memories>") {
			memAt = i
		}
	}
	if taskAt < 0 {
		t.Fatal("system 里找不到任务指令块")
	}
	if memAt < 0 {
		t.Fatal("体系前提不成立: 记忆块没有被注入, 本用例量不到要量的顺序 " +
			"(检查 MemoryStore 是否接上、RefreshHooks 是否补过)")
	}
	if memAt < taskAt {
		t.Fatalf("记忆块排在了任务指令**之前** (memory@%d, task@%d) —— "+
			"记忆块每次 run 都漂, 它一变就让后面任务指令那段大块连同整条前缀一起作废",
			memAt, taskAt)
	}
	if memAt != len(payload.System)-1 {
		t.Errorf("记忆块不是 system 的最后一块 (memory@%d, 共 %d 块) —— "+
			"易变的块必须压在最末尾, 后面再跟任何内容都是白付的缓存",
			memAt, len(payload.System))
	}
}

// TestLargeToolResultFoldedWithFileRef 是文件引用的**端到端**判据: 一条巨大的
// tool_result 经过真实引擎的 PhasePreCompact 后, 出站请求体里应出现可回读的
// 路径, 且该路径按引擎工作目录解析后**确实能读到完整原文**。
//
// 为什么必须走真引擎而不是直接调 compact 包: 折叠发生在 hook 链里, 而目录来自
// Config.Cwd —— 接线错了 (hook 拿到空 cwd、或 Compactor 与 hook 用了不同目录)
// 单测全绿, 线上却只落一半。这条把"配置 -> 目录 -> 落盘 -> 占位"整条链拉直。
func TestLargeToolResultFoldedWithFileRef(t *testing.T) {
	workdir := t.TempDir()

	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		resp := types.APIResponse{
			ID: "msg_1", Type: "message", Role: "assistant", Model: "test-model",
			Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: "ok"}},
			StopReason: "end_turn",
			Usage:      &types.Usage{InputTokens: 1, OutputTokens: 1},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "test-model", Client: srv.Client()}
	e := NewQueryEngine(&Config{MaxTurns: 1, Cwd: workdir}, client, tool.NewRegistry(), nil, nil, nil, prompt.NewManager(t.TempDir()))

	// 造一条远超折叠阈值的 tool_result, 内容可辨认以便回读比对。
	//
	// 必须配一条 assistant 的 tool_use: 孤儿 tool_result 会被 sanitizeToolPairing
	// 整条丢掉, 那样测的就成了"消息被丢弃"而不是"消息被折叠" —— 两条路径的
	// 出站请求体都读不到原文, 断言会以错误的原因通过。
	original := "ORIGINAL-PAYLOAD-" + strings.Repeat("Z", compact.MicroCompactThreshold+5000)
	e.Messages = []types.Message{
		{
			Type: types.MessageTypeAssistant,
			Content: []types.ContentBlock{{
				// tool_use 的 id 字段是 ID; ToolUseID 是 tool_result 侧的字段。
				// 写错这一个字段名, sanitizeToolPairing 就会把这对外科手术式地
				// 删掉, 而断言只会看到"原文不在请求体里"并以错误的原因变绿。
				Type: types.ContentBlockToolUse, ID: "call_big", Name: "Read",
			}},
		},
		{
			Type: types.MessageTypeUser,
			Content: []types.ContentBlock{{
				Type: types.ContentBlockToolResult, ToolUseID: "call_big", Content: original,
			}},
		},
	}

	// SentUpTo=0 -> 全部可改 (单轮场景, 与 MicroCompact 的 protectFrom 语义一致)。
	for range e.SubmitMessage(context.Background(), "") {
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("没有捕获到任何请求体")
	}
	body := bodies[0]

	if strings.Contains(body, original) {
		t.Fatal("完整原文仍出现在请求体里 —— 压根没折叠, 上下文没压下去")
	}
	if !strings.Contains(body, "已折叠") {
		t.Fatalf("未见折叠占位:\n%s", truncateForTest(body, 500))
	}

	// 占位里给出的路径必须真能读回完整原文。
	refDir := filepath.Join(workdir, compact.ToolResultRefDir())
	entries, err := os.ReadDir(refDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("折叠原文没落盘到 %s (err=%v) —— 占位里的引用是空话", refDir, err)
	}
	found := false
	for _, entry := range entries {
		back, err := os.ReadFile(filepath.Join(refDir, entry.Name()))
		if err != nil {
			continue
		}
		if string(back) == original {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("落盘的 %d 个文件里没有一份等于完整原文 —— 引用读不回原内容", len(entries))
	}
}

// 折叠的**可达性**是接线问题, 不是逻辑问题 —— 而接线错了两边的单测都绿。
//
// 背景: 大工具结果有两条落盘链。执行期溢出 (pkg/tool) 在 RunTools 里、阈值被引擎设成
// 16000; 压缩期折叠 (pkg/compact) 在 HookChain 的 PhasePreCompact 里、阈值同为 16000。
// 两者同阈值且溢出**先跑**, 所以任何被工具层折过的结果都到不了折叠层 —— 折叠在生产
// 只剩一条可达路径: **回读落盘 artifact** (自指保护让结果原样返回、不溢出)。
//
// 于是"压缩期折叠会给文件引用"这件事, 完全取决于那个 hook 有没有被装上。走
// RunIsolated 的路径 (CLI cliAgentRunner / 团队 stage 的内层 runner) **不过 HookChain**,
// 折叠在那里根本不执行; 走 SubmitMessage 的路径 (飞书团队 stage 主引擎) 才执行。
//
// 这条断言把"引擎注册了折叠 hook"钉住: 没有它, 把 registerInternalHooks 弄丢、
// 或把 MicroCompactHook 从链上摘掉, 都不影响任何折叠逻辑的测试 —— 它们直接调
// compact 包 —— 却让线上文件引用静默消失。
func TestEngineRegistersMicroCompactHookWithRefStore(t *testing.T) {
	workdir := t.TempDir()
	client := &api.Client{BaseURL: "http://127.0.0.1:1", APIKey: "k", Model: "m"}
	e := NewQueryEngine(&Config{MaxTurns: 1, Cwd: workdir}, client, tool.NewRegistry(), nil, nil, nil, prompt.NewManager(t.TempDir()))

	names := e.HookChain.Names(internal_hook.PhasePreCompact)
	found := false
	for _, n := range names {
		if n == "micro_compact" {
			found = true
		}
	}
	if !found {
		t.Fatalf("PhasePreCompact 里没有 micro_compact —— 压缩期折叠不会执行, "+
			"文件引用在生产静默消失 (现有折叠测试直接调 compact 包, 测不到这一层)。已注册: %v", names)
	}

	// 光注册不够, 还得带上落盘存储 —— cwd 为空时存储是 nil, 折叠退化为纯截断,
	// "引用"照样是句空话。用真引擎跑一轮, 断言占位里**确实**给出了可读回的路径:
	// 这是端到端判据, 比反射读私有字段稳, 也顺带覆盖 hook → 存储 → 落盘整条线。
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.APIResponse{
			ID: "m", Type: "message", Role: "assistant", Model: "m",
			Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: "ok"}},
			StopReason: "end_turn",
			Usage:      &types.Usage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer srv.Close()

	client2 := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client()}
	e2 := NewQueryEngine(&Config{MaxTurns: 1, Cwd: workdir}, client2, tool.NewRegistry(), nil, nil, nil, prompt.NewManager(t.TempDir()))
	big := "REF-WIRING-" + strings.Repeat("K", compact.MicroCompactThreshold+2000)
	e2.Messages = []types.Message{
		{Type: types.MessageTypeAssistant, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolUse, ID: "cw", Name: "Bash"},
		}},
		{Type: types.MessageTypeUser, Content: []types.ContentBlock{
			{Type: types.ContentBlockToolResult, ToolUseID: "cw", Content: big},
		}},
	}
	for range e2.SubmitMessage(context.Background(), "") {
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("没有捕获到请求体")
	}
	if !strings.Contains(bodies[0], "full_artifact: "+workdir) {
		t.Fatalf("占位里没有指向工作区的 folded-reference 路径 —— " +
			"hook 装上了但存储是 nil (cwd 没接线), 折叠静默退化为纯截断")
	}
}

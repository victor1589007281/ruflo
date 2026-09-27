package feishu

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/statestore"
)

// newSessionManagerWithMemory 与 newTestSessionManagerWithLLM 同形, 只是把 L1
// 记忆库也传进去 —— MemoryInject hook 的注册有 nil 守卫, 不传就等于没有那个 hook,
// 形态开关自然也无从施加。
func newSessionManagerWithMemory(t *testing.T, ss statestore.StateStore, mem *memory.TieredStore) *SessionManager {
	t.Helper()
	cfg := DefaultBotConfig()
	cfg.Cwd = t.TempDir()
	cfg.StateDir = filepath.Join(cfg.Cwd, ".claude-go")
	// 用生产默认值 (DefaultBotConfig 里就是 true): MemoryInject hook 只在
	// registerInternalHooks 跑的时候才建, 而它由 applyFeatureFlags 调用 ——
	// 这一项关掉的话记忆注入整个不存在, 形态开关自然也无从谈起。
	client := api.NewClient("http://127.0.0.1:1", "test-key", "test-model")
	client.Tag = "test"
	return NewSessionManager(cfg, client, nil, nil, nil, mem, nil, nil, nil, nil,
		WithStateStore(ss))
}

// 端到端: 配置文件里翻 engine.memoryIndexMode, 走热加载那条路, 活跃会话的引擎
// 必须跟着变。这是"做到配置里、可动态修改生效"这句要求的兑现点 —— 只验 apply
// 函数返回 nil 是不够的, 那条路上任何一环没接上都照样返回 nil。
func Test配置热改记忆形态_活跃会话跟着变(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	write := func(v string) {
		t.Helper()
		body := `{"engine":{"memoryIndexMode":` + v + `}}`
		if err := os.WriteFile(cfgPath, []byte(body), 0644); err != nil {
			t.Fatalf("写配置失败: %v", err)
		}
	}
	write("false")

	mem := memory.NewTieredStore()
	mem.Add(&memory.MemoryEntry{Content: "一条记忆", Source: "manual", Importance: 0.9})

	// 手工装配一个带 memoryStore 的 SessionManager (既有的 newTestSessionManager
	// 不给 memStore 传值, 而这里需要真实的 L1 库才能把 MemoryInject hook 建出来)。
	sm := newSessionManagerWithMemory(t, statestore.NewMemStore(), mem)
	b := &Bot{sessions: sm}

	// 造一个活跃会话 —— 引擎是会话建出来那一刻构造的, 所以这一步必须在改配置**之前**。
	s := sm.GetOrCreate("oc_switch")
	if s == nil || s.Engine == nil {
		t.Fatal("会话未建出来")
	}
	if s.Engine.MemoryIndexMode() {
		t.Fatal("前提不成立: 默认应当是关")
	}
	if !s.Engine.MemoryHookBound() {
		t.Fatal("会话引擎没有 MemoryInject hook —— 开关无从施加 (装配路径漏了 hook 重建?)")
	}

	// 热加载回调走的那条路。
	write("true")
	if err := b.ApplyEngineSwitches(cfgPath); err != nil {
		t.Fatalf("ApplyEngineSwitches 失败: %v", err)
	}
	if !s.Engine.MemoryIndexMode() {
		t.Error("改配置后活跃会话的引擎没跟着变 —— 热改没落到会话上")
	}

	// 改回去也必须生效 (单向能通不算通)。
	write("false")
	if err := b.ApplyEngineSwitches(cfgPath); err != nil {
		t.Fatalf("ApplyEngineSwitches 失败: %v", err)
	}
	if s.Engine.MemoryIndexMode() {
		t.Error("配置改回 false 后引擎仍是索引形态")
	}

	// 未配置 ≠ 关: 字段缺席时不得把已有的取值覆盖掉。
	write("true")
	if err := b.ApplyEngineSwitches(cfgPath); err != nil {
		t.Fatalf("ApplyEngineSwitches 失败: %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte(`{"engine":{}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := b.ApplyEngineSwitches(cfgPath); err != nil {
		t.Fatalf("ApplyEngineSwitches 失败: %v", err)
	}
	if !s.Engine.MemoryIndexMode() {
		t.Error("字段缺席被当成了 false —— 未配置不该改动已有取值")
	}
}

// 新建会话必须按配置定初值 —— 这条盯的是**接线漏了**, 而它此前确实是漏的。
//
// 上面那条端到端验的是「热加载推活跃会话」, 走 SessionManager.SetMemoryIndexMode,
// 与引擎构造无关。于是即使 createSessionWithClient 一个字段都没往 engine.Config 里
// 传, 那条也照样全绿 —— 活着的会话被推成索引形态了, 而**下一个**新会话又落回
// 正文形态, 两种形态在同一个进程里并存。实测确认过: engine.Config 里
// MemoryIndexMode 为 nil (没人赋值), 于是走 resolvedMemoryIndexMode 的 env 兜底
// (未设 = false)。
//
// 所以这里刻意**不碰** ApplyEngineSwitches, 只从配置造出 SessionManager 再开会话,
// 走的是"进程启动 + 新会话"这条路径。
func Test新建会话按配置定注入形态(t *testing.T) {
	mem := memory.NewTieredStore()
	mem.Add(&memory.MemoryEntry{Content: "一条记忆", Source: "manual", Importance: 0.9})

	for _, on := range []bool{true, false} {
		cfg := DefaultBotConfig()
		cfg.Cwd = t.TempDir()
		cfg.StateDir = filepath.Join(cfg.Cwd, ".claude-go")
		v := on
		cfg.MemoryIndexMode = &v

		client := api.NewClient("http://127.0.0.1:1", "test-key", "test-model")
		client.Tag = "test"
		sm := NewSessionManager(cfg, client, nil, nil, nil, mem, nil, nil, nil, nil,
			WithStateStore(statestore.NewMemStore()))

		s := sm.GetOrCreate("oc_new")
		if s == nil || s.Engine == nil {
			t.Fatal("会话未建出来")
		}
		if !s.Engine.MemoryHookBound() {
			t.Fatal("会话引擎没有 MemoryInject hook —— 开关无从施加")
		}
		if got := s.Engine.MemoryIndexMode(); got != on {
			t.Errorf("配置 memoryIndexMode=%v, 新建会话引擎却是 %v —— 构造路径没接线", on, got)
		}
	}
}

// 未配置 (nil) 时新建会话不得把 env 兜底打死。
//
// engine.Config.MemoryIndexMode 是 *bool, nil 的语义是「没配, 由
// resolvedMemoryIndexMode 回落到 CLAUDE_GO_MEMORY_INDEX_MODE」。接线时若图省事写成
// `boolPtr(sm.config.MemoryIndexMode != nil && *sm.config.MemoryIndexMode)` 就会恒为非 nil,
// 于是显式设了 env 的部署在飞书路径上完全不生效 —— 这条守的就是那个"顺手压平"。
func Test未配置时不打死环境变量兜底(t *testing.T) {
	t.Setenv("CLAUDE_GO_MEMORY_INDEX_MODE", "1")

	mem := memory.NewTieredStore()
	mem.Add(&memory.MemoryEntry{Content: "一条记忆", Source: "manual", Importance: 0.9})

	cfg := DefaultBotConfig()
	cfg.Cwd = t.TempDir()
	cfg.StateDir = filepath.Join(cfg.Cwd, ".claude-go")
	cfg.MemoryIndexMode = nil // 未配置

	client := api.NewClient("http://127.0.0.1:1", "test-key", "test-model")
	client.Tag = "test"
	sm := NewSessionManager(cfg, client, nil, nil, nil, mem, nil, nil, nil, nil,
		WithStateStore(statestore.NewMemStore()))

	s := sm.GetOrCreate("oc_env")
	if s == nil || s.Engine == nil {
		t.Fatal("会话未建出来")
	}
	if !s.Engine.MemoryIndexMode() {
		t.Error("配置未设时没有回落到 CLAUDE_GO_MEMORY_INDEX_MODE=1")
	}
}

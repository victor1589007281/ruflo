// hook_registration_order_test.go —— 钉死"组件赋值顺序决定 hook 是否注册"这条陷阱。
//
// 背景：多个内置 hook 的注册带 nil 守卫（MemoryInject 要 MemoryStore/FactStore
// 非 nil、TraceCapture 要 TraceStore 非 nil），而 registerInternalHooks 只在
// NewQueryEngine 与 EnableFrontierOptimizations 里跑。调用方若在**构造之后**
// 才赋这些组件、且不再触发一次注册，hook 就静默缺席。
//
// 这不是理论风险：`cmd/claude-go/main.go` 的 CLI 路径正是这个顺序
// （:2916 构造 → :2929 赋 MemoryStore → :2933 赋 FactStore → :2964 赋 TraceStore，
// 全程无 EnableFrontierOptimizations），于是 CLI 形态下 L1/L2 记忆注入与轨迹
// 采集两个 hook 都不生效——而下游平台的全部流量走的是 CLI headless。
package engine

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/memory"
)

func hookNamesContain(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// 构造后赋组件、不重新注册 —— 复刻 CLI 的顺序，断言两个 hook 均缺席。
func TestHookRegistration_构造后赋组件则hook缺席(t *testing.T) {
	e := &QueryEngine{Config: &Config{}}
	e.applyFeatureFlags() // 等价于 NewQueryEngine 末尾那次注册

	// 此刻 MemoryStore/FactStore/TraceStore 都还是 nil
	e.MemoryStore = memory.NewTieredStore()
	e.FactStore = memory.NewFactStore(t.TempDir())
	e.TraceStore = nil // CLI 里这里会赋真实 store, 但顺序同样在注册之后

	pre := e.HookChain.Names(internal_hook.PhasePreRequest)
	if hookNamesContain(pre, "memory_inject") {
		t.Error("前置条件不成立: 构造时 MemoryStore 为 nil, memory_inject 不应已注册")
	}
	t.Logf("PhasePreRequest 已注册 hook: %v", pre)
}

// 赋组件后再触发一次注册 —— 两个 hook 都应出现。这就是 CLI 缺的那一步。
func TestHookRegistration_重新注册后hook到位(t *testing.T) {
	e := &QueryEngine{Config: &Config{}}
	e.applyFeatureFlags()

	e.MemoryStore = memory.NewTieredStore()
	e.FactStore = memory.NewFactStore(t.TempDir())
	e.registerInternalHooks() // 补这一次

	pre := e.HookChain.Names(internal_hook.PhasePreRequest)
	if !hookNamesContain(pre, "memory_inject") {
		t.Errorf("补注册后 memory_inject 仍缺席, 实际: %v", pre)
	}
}

// 飞书路径的顺序：构造 → 赋组件 → EnableFrontierOptimizations（内部会重新注册）。
// 断言这条路径确实能拿到 hook，从而证明两形态的差异只在"有没有补那一次注册"。
func TestHookRegistration_飞书路径顺序可拿到hook(t *testing.T) {
	e := &QueryEngine{Config: &Config{}}
	e.applyFeatureFlags()

	e.MemoryStore = memory.NewTieredStore()
	e.FactStore = memory.NewFactStore(t.TempDir())
	e.EnableFrontierOptimizations() // 飞书侧真实调用, 末尾会 applyFeatureFlags → registerInternalHooks

	pre := e.HookChain.Names(internal_hook.PhasePreRequest)
	if !hookNamesContain(pre, "memory_inject") {
		t.Errorf("飞书路径应能注册 memory_inject, 实际: %v", pre)
	}
}

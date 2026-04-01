// Package hooks 实现 Ruflo 编排钩子子系统。
//
// 整体架构（与 executor、workers、llm_hooks、reasoningbank 等协同）：
//   - 注册中心（本文件）：观察者模式，按 HookEvent 维护回调列表，并以全局唯一 Name 索引；支持 Enable/Disable 而不卸载 Handler。
//   - 执行器（executor.go）：责任链模式，按优先级顺序调用，可合并 Data/Warnings，遇 Abort 或错误提前终止。
//   - 后台 Worker（workers*.go）：由触发字符串正则匹配分发到 12 类专用任务。
//   - LLM 钩子（llm_hooks.go）：请求/响应拦截，LRU 缓存与指标。
//   - 推理银行（reasoningbank.go）：经验模式存储与任务路由。
package hooks

import (
	"sort"
	"sync"
)

// HookRegistryStats 汇总注册表统计信息，用于观测与调试。
type HookRegistryStats struct {
	TotalRegistered int               // 已注册钩子总数（含禁用）
	EnabledCount    int               // 当前处于启用状态的钩子数
	DisabledCount   int               // 当前处于禁用状态的钩子数
	EventCounts     map[HookEvent]int // 各事件类型下的注册数量（按名称去重后的事件归属）
}

// HookRegistry 钩子注册中心：按事件类型分桶存储，同时维护按唯一名称的快速查找表。
// 设计要点：同一 Name 全局唯一；GetForEvent 仅返回 Enabled 的项并按 Priority 降序（同优先级按 Name 字典序）稳定排序。
type HookRegistry struct {
	mu      sync.RWMutex                     // 保护 byEvent、byName 的读写锁
	byEvent map[HookEvent][]HookRegistration // 事件 -> 该事件下所有注册（含禁用项，由 GetForEvent 过滤）
	byName  map[string]HookRegistration      // 唯一名称 -> 注册副本（Enable/Disable 时同步更新两处）
}

// NewRegistry 创建一个空的注册表，内部初始化两个 map。
func NewRegistry() *HookRegistry {
	return &HookRegistry{
		byEvent: make(map[HookEvent][]HookRegistration),
		byName:  make(map[string]HookRegistration),
	}
}

// Register 注册钩子：handler 与 name 必填；name 在所有事件中必须唯一。
// 算法：在 byName 中查重后，写入 byName 并将 reg 追加到 byEvent[event]（同一事件内顺序由 GetForEvent 时再排序）。
func (r *HookRegistry) Register(event HookEvent, handler HookHandler, priority int, name string) error {
	if handler == nil || name == "" {
		return ErrInvalidRegistration
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[name]; exists {
		return ErrDuplicateName
	}
	reg := HookRegistration{Event: event, Handler: handler, Priority: priority, Name: name, Enabled: true}
	r.byName[name] = reg
	r.byEvent[event] = append(r.byEvent[event], reg)
	return nil
}

// Unregister 按唯一名称移除钩子：从 byName 删除，并在对应事件的切片中原地压缩（slice 重用底层数组 [:0] 技巧）。
func (r *HookRegistry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.byName[name]
	if !ok {
		return false
	}
	delete(r.byName, name)
	list := r.byEvent[reg.Event]
	out := list[:0]
	for _, x := range list {
		if x.Name != name {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		delete(r.byEvent, reg.Event)
	} else {
		r.byEvent[reg.Event] = out
	}
	return true
}

// GetForEvent 返回某事件下所有已启用钩子，按 Priority 降序（数值大先执行），同优先级按 Name 升序。
// 算法：拷贝过滤后 sort.Slice，不修改注册表内部存储顺序。
func (r *HookRegistry) GetForEvent(event HookEvent) []HookRegistration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := r.byEvent[event]
	var cp []HookRegistration
	for _, reg := range list {
		if reg.Enabled {
			cp = append(cp, reg)
		}
	}
	sort.Slice(cp, func(i, j int) bool {
		if cp[i].Priority != cp[j].Priority {
			return cp[i].Priority > cp[j].Priority
		}
		return cp[i].Name < cp[j].Name
	})
	return cp
}

// Get 按唯一名称返回注册信息副本（避免调用方持有内部指针导致数据竞争）。
func (r *HookRegistry) Get(name string) (*HookRegistration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.byName[name]
	if !ok {
		return nil, false
	}
	cp := reg
	return &cp, true
}

// Enable 按名称启用钩子：更新 byName 与 byEvent 中对应项的 Enabled 字段（syncEnabledInEventListLocked）。
func (r *HookRegistry) Enable(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.byName[name]
	if !ok {
		return ErrHookNotFound
	}
	reg.Enabled = true
	r.byName[name] = reg
	r.syncEnabledInEventListLocked(reg.Event, name, true)
	return nil
}

// Disable 按名称禁用钩子：保留注册但 GetForEvent 不再返回该项。
func (r *HookRegistry) Disable(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.byName[name]
	if !ok {
		return ErrHookNotFound
	}
	reg.Enabled = false
	r.byName[name] = reg
	r.syncEnabledInEventListLocked(reg.Event, name, false)
	return nil
}

// syncEnabledInEventListLocked 在已持锁前提下，将某事件切片中与 name 匹配的项的 Enabled 与主索引一致。
func (r *HookRegistry) syncEnabledInEventListLocked(ev HookEvent, name string, enabled bool) {
	list := r.byEvent[ev]
	for i := range list {
		if list[i].Name == name {
			list[i].Enabled = enabled
			r.byEvent[ev] = list
			return
		}
	}
}

// Has 判断某名称是否已注册（无论启用与否）。
func (r *HookRegistry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.byName[name]
	return ok
}

// Size 返回已注册钩子数量（byName 长度）。
func (r *HookRegistry) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byName)
}

// Clear 清空全部注册（替换为新 map，避免残留键）。
func (r *HookRegistry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName = make(map[string]HookRegistration)
	r.byEvent = make(map[HookEvent][]HookRegistration)
}

// GetStats 遍历 byName 统计启用/禁用数量及按事件计数。
func (r *HookRegistry) GetStats() HookRegistryStats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st := HookRegistryStats{
		TotalRegistered: len(r.byName),
		EventCounts:     make(map[HookEvent]int),
	}
	for _, reg := range r.byName {
		if reg.Enabled {
			st.EnabledCount++
		} else {
			st.DisabledCount++
		}
		st.EventCounts[reg.Event]++
	}
	return st
}

// List 返回全部注册副本，按 Name 排序，顺序确定便于测试与展示。
func (r *HookRegistry) List() []HookRegistration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]HookRegistration, 0, len(r.byName))
	for _, reg := range r.byName {
		out = append(out, reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// 控制器注册表（memory 包）：登记多个 AgentDB/子系统控制器，统一 Initialize、HealthCheck、Close；initOrder 保证启动顺序与关闭逆序。
// HealthCheckAll 按名称排序遍历，结果确定性便于对拍测试；GetController/ListControllers 用于运行时查找。
package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Controller is an initializable subsystem with health and shutdown hooks.
type Controller interface {
	Name() string
	Initialize(ctx context.Context) error
	HealthCheck(ctx context.Context) error
	Close() error
}

// ControllerRegistry manages ordered controller lifecycle.
type ControllerRegistry struct {
	mu            sync.RWMutex
	controllers   map[string]Controller
	initOrder     []string
	initialized   map[string]bool
	initializeErr map[string]error
}

// NewControllerRegistry constructs an empty registry.
func NewControllerRegistry() *ControllerRegistry {
	return &ControllerRegistry{
		controllers:   make(map[string]Controller),
		initialized:   make(map[string]bool),
		initializeErr: make(map[string]error),
	}
}

// Register adds a controller; name must match Controller.Name() and be unique.
func (r *ControllerRegistry) Register(c Controller) error {
	if r == nil || c == nil {
		return fmt.Errorf("controller registry: nil controller")
	}
	name := c.Name()
	if name == "" {
		return fmt.Errorf("controller registry: empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.controllers[name]; exists {
		return fmt.Errorf("controller registry: duplicate %q", name)
	}
	r.controllers[name] = c
	r.initOrder = append(r.initOrder, name)
	return nil
}

// InitializeAll runs Initialize in registration order.
func (r *ControllerRegistry) InitializeAll(ctx context.Context) error {
	if r == nil {
		return fmt.Errorf("controller registry: nil")
	}
	r.mu.Lock()
	order := append([]string(nil), r.initOrder...)
	r.mu.Unlock()
	for _, name := range order {
		r.mu.RLock()
		c := r.controllers[name]
		r.mu.RUnlock()
		if c == nil {
			continue
		}
		if err := c.Initialize(ctx); err != nil {
			r.mu.Lock()
			r.initializeErr[name] = err
			r.mu.Unlock()
			return fmt.Errorf("controller %q: %w", name, err)
		}
		r.mu.Lock()
		r.initialized[name] = true
		delete(r.initializeErr, name)
		r.mu.Unlock()
	}
	return nil
}

// HealthCheckAll runs HealthCheck on all registered controllers (unordered by name sort for stability).
func (r *ControllerRegistry) HealthCheckAll(ctx context.Context) error {
	if r == nil {
		return fmt.Errorf("controller registry: nil")
	}
	r.mu.RLock()
	names := make([]string, 0, len(r.controllers))
	for n := range r.controllers {
		names = append(names, n)
	}
	r.mu.RUnlock()
	sort.Strings(names)
	var first error
	for _, name := range names {
		r.mu.RLock()
		c := r.controllers[name]
		r.mu.RUnlock()
		if c == nil {
			continue
		}
		if err := c.HealthCheck(ctx); err != nil && first == nil {
			first = fmt.Errorf("controller %q: %w", name, err)
		}
	}
	return first
}

// CloseAll closes controllers in reverse registration order.
func (r *ControllerRegistry) CloseAll() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	order := append([]string(nil), r.initOrder...)
	r.mu.Unlock()
	var first error
	for i := len(order) - 1; i >= 0; i-- {
		name := order[i]
		r.mu.RLock()
		c := r.controllers[name]
		r.mu.RUnlock()
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil && first == nil {
			first = fmt.Errorf("controller %q close: %w", name, err)
		}
	}
	return first
}

// GetController returns a registered controller by name.
func (r *ControllerRegistry) GetController(name string) (Controller, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.controllers[name]
	return c, ok
}

// ListControllers 返回字典序排序的全部注册名。
func (r *ControllerRegistry) ListControllers() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.controllers))
	for n := range r.controllers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

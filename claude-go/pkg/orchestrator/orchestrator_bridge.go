package orchestrator

import (
	"time"

	"github.com/anthropic/claude-go/pkg/observability"
)

// ObservabilityBridge 将 orchestrator 的 LifecycleHook 桥接到 observability 事件总线。
// 实现 LifecycleHook 接口, 在 Engine.SetHook 中使用。
type ObservabilityBridge struct{}

var _ LifecycleHook = (*ObservabilityBridge)(nil)

// NewObservabilityBridge 创建桥接 hook。
func NewObservabilityBridge() *ObservabilityBridge {
	return &ObservabilityBridge{}
}

func (b *ObservabilityBridge) OnGraphStart(g *Graph) {
	observability.Emit(observability.Event{
		Type:      observability.EvtTeamStart,
		Timestamp: time.Now(),
		Module:    "orchestrator",
		Name:      g.Name,
		Payload: map[string]interface{}{
			"graph_id":   g.ID,
			"graph_name": g.Name,
			"node_count": len(g.Tasks),
		},
	})
}

func (b *ObservabilityBridge) OnTaskReady(t *Task) {
	observability.Emit(observability.Event{
		Type:      observability.EvtTaskReady,
		Timestamp: time.Now(),
		Module:    "orchestrator",
		Name:      t.Name,
		Payload: map[string]interface{}{
			"task_id":      t.ID,
			"task_name":    t.Name,
			"dependencies": t.DependsOn,
		},
	})
}

func (b *ObservabilityBridge) OnTaskStart(t *Task) {
	observability.Emit(observability.Event{
		Type:      observability.EvtTaskStart,
		Timestamp: time.Now(),
		Module:    "orchestrator",
		Name:      t.Name,
		Payload: map[string]interface{}{
			"task_id":   t.ID,
			"task_name": t.Name,
			"attempt":   t.Retries(),
		},
	})
}

func (b *ObservabilityBridge) OnTaskComplete(t *Task, output any) {
	observability.Emit(observability.Event{
		Type:      observability.EvtTaskComplete,
		Timestamp: time.Now(),
		Module:    "orchestrator",
		Name:      t.Name,
		Payload: map[string]interface{}{
			"task_id":     t.ID,
			"task_name":   t.Name,
			"success":     true,
			"duration_sec": 0.0,
		},
	})
}

func (b *ObservabilityBridge) OnTaskFailed(t *Task, err error) {
	observability.Emit(observability.Event{
		Type:      observability.EvtTaskFail,
		Timestamp: time.Now(),
		Module:    "orchestrator",
		Name:      t.Name,
		Payload: map[string]interface{}{
			"task_id":   t.ID,
			"task_name": t.Name,
			"success":   false,
			"error":     err.Error(),
			"attempt":   t.Retries(),
		},
	})
}

func (b *ObservabilityBridge) OnTaskRetry(t *Task, attempt int, delay interface{}) {
	observability.Emit(observability.Event{
		Type:      observability.EvtTaskRetry,
		Timestamp: time.Now(),
		Module:    "orchestrator",
		Name:      t.Name,
		Payload: map[string]interface{}{
			"task_id":   t.ID,
			"task_name": t.Name,
			"attempt":   attempt,
			"delay_ms":  delay,
		},
	})
}

func (b *ObservabilityBridge) OnTaskSuspended(t *Task, err error) {}

func (b *ObservabilityBridge) OnGraphComplete(g *Graph, metrics ExecutionMetrics) {
	observability.Emit(observability.Event{
		Type:      observability.EvtTeamComplete,
		Timestamp: time.Now(),
		Module:    "orchestrator",
		Name:      g.Name,
		Payload: map[string]interface{}{
			"graph_id":       g.ID,
			"graph_name":     g.Name,
			"completed":      metrics.CompletedTasks,
			"failed":         metrics.FailedTasks,
			"total_duration": metrics.Elapsed.Seconds(),
		},
	})
}

func (b *ObservabilityBridge) OnStallDetected(g *Graph, stuckTasks []*Task) {
	observability.Emit(observability.Event{
		Type:      observability.EvtTeamFail,
		Timestamp: time.Now(),
		Module:    "orchestrator",
		Name:      g.Name,
		Payload: map[string]interface{}{
			"graph_id":     g.ID,
			"graph_name":   g.Name,
			"event":        "stall_detected",
			"stuck_count":  len(stuckTasks),
		},
	})
}

func (b *ObservabilityBridge) OnCheckpoint(execID string, state *ExecutionState) {}

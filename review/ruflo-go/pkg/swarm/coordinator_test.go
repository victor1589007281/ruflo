package swarm

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

func testCoordinator(t *testing.T) (*UnifiedSwarmCoordinator, func()) {
	t.Helper()
	cfg := CoordinatorConfig{
		Topology:        api.TopologyMesh,
		ConsensusAlgo:   api.ConsensusRaft,
		AgentPoolMin:    1,
		AgentPoolMax:    4,
		HeartbeatMS:     500,
		MetricsInterval: time.Hour,
	}
	c := NewUnifiedSwarmCoordinator(cfg)
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	done := func() {
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Shutdown(shCtx)
	}
	return c, done
}

func registerCoder(t *testing.T, c *UnifiedSwarmCoordinator, id string, state api.AgentState) {
	t.Helper()
	ag := &api.Agent{
		ID:        id,
		Type:      api.AgentTypeCoder,
		Domain:    api.AgentDomainCore,
		State:     state,
		Status:    api.AgentStatusHealthy,
		CreatedAt: time.Now(),
		Capabilities: api.AgentCapabilities{
			Skills: []string{string(api.TaskTypeCoding)},
		},
	}
	if err := c.RegisterAgent(ag); err != nil {
		t.Fatal(err)
	}
}

func TestPauseResume(t *testing.T) {
	c, done := testCoordinator(t)
	defer done()
	registerCoder(t, c, "worker-pause", api.AgentStateIdle)
	task := &api.TaskDefinition{ID: "t-pause", Type: api.TaskTypeCoding, Title: "x"}
	c.Pause()
	if err := c.SubmitTask(task); err == nil {
		t.Fatal("expected error when paused")
	}
	c.Resume()
	if err := c.SubmitTask(task); err != nil {
		t.Fatalf("SubmitTask after resume: %v", err)
	}
}

func TestGetAgent(t *testing.T) {
	c, done := testCoordinator(t)
	defer done()
	registerCoder(t, c, "ga-1", api.AgentStateIdle)
	a, ok := c.GetAgent("ga-1")
	if !ok || a == nil || a.ID != "ga-1" {
		t.Fatalf("GetAgent: ok=%v %#v", ok, a)
	}
}

func TestGetAllAgents(t *testing.T) {
	c, done := testCoordinator(t)
	defer done()
	registerCoder(t, c, "all-a", api.AgentStateIdle)
	registerCoder(t, c, "all-b", api.AgentStateIdle)
	all := c.GetAllAgents()
	if len(all) < 2 {
		t.Fatalf("GetAllAgents: %d", len(all))
	}
}

func TestGetAgentsByType(t *testing.T) {
	c, done := testCoordinator(t)
	defer done()
	registerCoder(t, c, "coder-x", api.AgentStateIdle)
	rev := &api.Agent{
		ID:        "rev-x",
		Type:      api.AgentTypeReviewer,
		Domain:    api.AgentDomainCore,
		State:     api.AgentStateIdle,
		Status:    api.AgentStatusHealthy,
		CreatedAt: time.Now(),
	}
	if err := c.RegisterAgent(rev); err != nil {
		t.Fatal(err)
	}
	coders := c.GetAgentsByType(api.AgentTypeCoder)
	if len(coders) < 1 {
		t.Fatalf("coders: %d", len(coders))
	}
}

func TestGetAvailableAgents(t *testing.T) {
	c, done := testCoordinator(t)
	defer done()
	registerCoder(t, c, "idle-1", api.AgentStateIdle)
	registerCoder(t, c, "busy-1", api.AgentStateBusy)
	avail := c.GetAvailableAgents()
	for _, a := range avail {
		if a.ID == "busy-1" {
			t.Fatal("busy agent should not be listed as available")
		}
	}
}

func TestCancelTask(t *testing.T) {
	c, done := testCoordinator(t)
	defer done()
	registerCoder(t, c, "cancel-agent", api.AgentStateIdle)
	task := &api.TaskDefinition{ID: "t-cancel", Type: api.TaskTypeCoding, Title: "c"}
	if err := c.SubmitTask(task); err != nil {
		t.Fatal(err)
	}
	if err := c.CancelTask("t-cancel"); err != nil {
		t.Fatal(err)
	}
	got, ok := c.GetTask("t-cancel")
	if !ok || got.Status != api.TaskStatusCancelled {
		t.Fatalf("task status: %#v", got)
	}
}

func TestBroadcastMessage(t *testing.T) {
	c, done := testCoordinator(t)
	defer done()
	registerCoder(t, c, "bc-a", api.AgentStateIdle)
	registerCoder(t, c, "bc-b", api.AgentStateIdle)
	bus := c.Bus()
	var mu sync.Mutex
	received := map[string]int{}
	for _, id := range []string{"bc-a", "bc-b"} {
		aid := id
		bus.Subscribe(aid, func(m api.Message) {
			if m.Type == api.MessageTypeEvent {
				mu.Lock()
				received[aid]++
				mu.Unlock()
			}
		}, nil)
	}
	msg := &api.Message{Type: api.MessageTypeEvent, Payload: map[string]any{"x": 1}}
	if err := c.BroadcastMessage(msg); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := received["bc-a"] >= 1 && received["bc-b"] >= 1
		mu.Unlock()
		if ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if received["bc-a"] < 1 || received["bc-b"] < 1 {
		t.Fatalf("broadcast delivery: %#v", received)
	}
}

func TestGetState(t *testing.T) {
	c, done := testCoordinator(t)
	defer done()
	st := c.GetState()
	if st.Topology != api.TopologyMesh {
		t.Fatalf("Topology: %v", st.Topology)
	}
	if st.Status != api.SwarmStatusHealthy {
		t.Fatalf("Status: %v", st.Status)
	}
}

func TestGetMetrics(t *testing.T) {
	c, done := testCoordinator(t)
	defer done()
	registerCoder(t, c, "met-1", api.AgentStateIdle)
	_ = c.SubmitTask(&api.TaskDefinition{ID: "met-task", Type: api.TaskTypeCoding, Title: "m"})
	m := c.GetMetrics()
	if m.TasksSubmitted < 1 || m.TasksAssigned < 1 {
		t.Fatalf("metrics: submitted=%d assigned=%d", m.TasksSubmitted, m.TasksAssigned)
	}
}

func TestIsHealthy(t *testing.T) {
	c, done := testCoordinator(t)
	defer done()
	if !c.IsHealthy() {
		t.Fatal("expected healthy after init")
	}
	c.Pause()
	if c.IsHealthy() {
		t.Fatal("expected not healthy when paused")
	}
	c.Resume()
	if !c.IsHealthy() {
		t.Fatal("expected healthy after resume")
	}
}

package api

import "testing"

func TestAgentTypeValues(t *testing.T) {
	t.Parallel()
	types := []AgentType{
		AgentTypeCoordinator,
		AgentTypeCoder,
		AgentTypeTester,
		AgentTypeReviewer,
		AgentTypeArchitect,
		AgentTypeResearcher,
		AgentTypeSecurityArchitect,
		AgentTypeSecurityAuditor,
		AgentTypeMemorySpecialist,
		AgentTypePerformanceEngineer,
		AgentTypePerformance,
		AgentTypePlanner,
		AgentTypeCustom,
		AgentTypeQueen,
	}
	seen := make(map[AgentType]struct{})
	for _, at := range types {
		if at == "" {
			t.Fatal("empty agent type")
		}
		if _, ok := seen[at]; ok {
			t.Fatalf("duplicate agent type %q", at)
		}
		seen[at] = struct{}{}
	}
}

func TestTaskStatusValues(t *testing.T) {
	t.Parallel()
	terminal := map[TaskStatus]struct{}{
		TaskStatusSucceeded: {},
		TaskStatusFailed:    {},
		TaskStatusCancelled: {},
		TaskStatusCompleted: {},
		TaskStatusTimeout:   {},
	}
	active := []TaskStatus{TaskStatusPending, TaskStatusQueued, TaskStatusAssigned, TaskStatusRunning, TaskStatusBlocked, TaskStatusRetrying}
	for _, s := range active {
		if _, ok := terminal[s]; ok {
			t.Fatalf("%s should not be both active-like and terminal", s)
		}
	}
	if TaskStatusPending == "" || TaskStatusRunning == "" {
		t.Fatal("core statuses must be non-empty strings")
	}
}

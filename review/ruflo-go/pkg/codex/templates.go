package codex

const defaultCollabNS = "collaboration"

// FeatureTemplate is architect → coder → tester → reviewer across Claude and Codex.
func FeatureTemplate(task string) []WorkerConfig {
	ns := defaultCollabNS
	return []WorkerConfig{
		{Platform: "claude", Role: "architect", Prompt: "Design the implementation for: " + task, Namespace: ns},
		{Platform: "codex", Role: "coder", Prompt: "Implement based on design: " + task, DependsOn: []string{"architect"}, Namespace: ns},
		{Platform: "claude", Role: "tester", Prompt: "Write tests and verify: " + task, DependsOn: []string{"coder"}, Namespace: ns},
		{Platform: "codex", Role: "reviewer", Prompt: "Review code quality and security: " + task, DependsOn: []string{"tester"}, Namespace: ns},
	}
}

// SecurityTemplate runs analysis → automated scan → report.
func SecurityTemplate(target string) []WorkerConfig {
	ns := defaultCollabNS
	return []WorkerConfig{
		{Platform: "claude", Role: "analyst", Prompt: "Threat model and security analysis for: " + target, Namespace: ns},
		{Platform: "codex", Role: "scanner", Prompt: "Scan and enumerate issues in: " + target, DependsOn: []string{"analyst"}, Namespace: ns},
		{Platform: "claude", Role: "reporter", Prompt: "Produce security report for: " + target, DependsOn: []string{"scanner"}, Namespace: ns},
	}
}

// RefactorTemplate plans → refactors → verifies.
func RefactorTemplate(target string) []WorkerConfig {
	ns := defaultCollabNS
	return []WorkerConfig{
		{Platform: "claude", Role: "architect", Prompt: "Plan refactor for: " + target, Namespace: ns},
		{Platform: "codex", Role: "refactorer", Prompt: "Apply refactor to: " + target, DependsOn: []string{"architect"}, Namespace: ns},
		{Platform: "claude", Role: "tester", Prompt: "Verify behavior after refactor: " + target, DependsOn: []string{"refactorer"}, Namespace: ns},
	}
}

// BugfixTemplate investigates → fixes → regression-tests.
func BugfixTemplate(bug string) []WorkerConfig {
	ns := defaultCollabNS
	return []WorkerConfig{
		{Platform: "claude", Role: "researcher", Prompt: "Investigate root cause: " + bug, Namespace: ns},
		{Platform: "codex", Role: "coder", Prompt: "Implement fix for: " + bug, DependsOn: []string{"researcher"}, Namespace: ns},
		{Platform: "claude", Role: "tester", Prompt: "Regression and verification for: " + bug, DependsOn: []string{"coder"}, Namespace: ns},
	}
}

// CollaborationTemplates provides the same pipelines as package-level template functions.
type CollaborationTemplates struct{}

func (CollaborationTemplates) Feature(task string) []WorkerConfig { return FeatureTemplate(task) }
func (CollaborationTemplates) Security(target string) []WorkerConfig {
	return SecurityTemplate(target)
}
func (CollaborationTemplates) Refactor(target string) []WorkerConfig {
	return RefactorTemplate(target)
}
func (CollaborationTemplates) Bugfix(bug string) []WorkerConfig { return BugfixTemplate(bug) }

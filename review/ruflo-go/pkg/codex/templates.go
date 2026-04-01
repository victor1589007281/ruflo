// 本文件提供预定义的跨平台协作模板（WorkerConfig 流水线）。
//
// 设计思路：将常见研发场景固化为「角色 × 平台 × 依赖」有向图，默认命名空间 collaboration，
// 与 DualModeOrchestrator.RunCollaboration 及共享记忆约定一致；Claude 侧重架构/测试/分析，Codex 侧重实现与扫描类自动化。
package codex

const defaultCollabNS = "collaboration"

// FeatureTemplate 特性开发：architect(Claude) → coder(Codex) → tester(Claude) → reviewer(Codex)。
func FeatureTemplate(task string) []WorkerConfig {
	ns := defaultCollabNS
	return []WorkerConfig{
		{Platform: "claude", Role: "architect", Prompt: "Design the implementation for: " + task, Namespace: ns},
		{Platform: "codex", Role: "coder", Prompt: "Implement based on design: " + task, DependsOn: []string{"architect"}, Namespace: ns},
		{Platform: "claude", Role: "tester", Prompt: "Write tests and verify: " + task, DependsOn: []string{"coder"}, Namespace: ns},
		{Platform: "codex", Role: "reviewer", Prompt: "Review code quality and security: " + task, DependsOn: []string{"tester"}, Namespace: ns},
	}
}

// SecurityTemplate 安全审计：analyst(Claude) → scanner(Codex) → reporter(Claude)。
func SecurityTemplate(target string) []WorkerConfig {
	ns := defaultCollabNS
	return []WorkerConfig{
		{Platform: "claude", Role: "analyst", Prompt: "Threat model and security analysis for: " + target, Namespace: ns},
		{Platform: "codex", Role: "scanner", Prompt: "Scan and enumerate issues in: " + target, DependsOn: []string{"analyst"}, Namespace: ns},
		{Platform: "claude", Role: "reporter", Prompt: "Produce security report for: " + target, DependsOn: []string{"scanner"}, Namespace: ns},
	}
}

// RefactorTemplate 重构：architect 规划 → refactorer(Codex) 落地 → tester 验证。
func RefactorTemplate(target string) []WorkerConfig {
	ns := defaultCollabNS
	return []WorkerConfig{
		{Platform: "claude", Role: "architect", Prompt: "Plan refactor for: " + target, Namespace: ns},
		{Platform: "codex", Role: "refactorer", Prompt: "Apply refactor to: " + target, DependsOn: []string{"architect"}, Namespace: ns},
		{Platform: "claude", Role: "tester", Prompt: "Verify behavior after refactor: " + target, DependsOn: []string{"refactorer"}, Namespace: ns},
	}
}

// BugfixTemplate 缺陷修复：researcher 定位 → coder 修复 → tester 回归验证。
func BugfixTemplate(bug string) []WorkerConfig {
	ns := defaultCollabNS
	return []WorkerConfig{
		{Platform: "claude", Role: "researcher", Prompt: "Investigate root cause: " + bug, Namespace: ns},
		{Platform: "codex", Role: "coder", Prompt: "Implement fix for: " + bug, DependsOn: []string{"researcher"}, Namespace: ns},
		{Platform: "claude", Role: "tester", Prompt: "Regression and verification for: " + bug, DependsOn: []string{"coder"}, Namespace: ns},
	}
}

// CollaborationTemplates 方法集与包级模板函数等价，便于以值接收者方式注入或满足接口。
type CollaborationTemplates struct{}

// Feature 同 FeatureTemplate。
func (CollaborationTemplates) Feature(task string) []WorkerConfig { return FeatureTemplate(task) }

// Security 同 SecurityTemplate。
func (CollaborationTemplates) Security(target string) []WorkerConfig {
	return SecurityTemplate(target)
}

// Refactor 同 RefactorTemplate。
func (CollaborationTemplates) Refactor(target string) []WorkerConfig {
	return RefactorTemplate(target)
}

// Bugfix 同 BugfixTemplate。
func (CollaborationTemplates) Bugfix(bug string) []WorkerConfig { return BugfixTemplate(bug) }

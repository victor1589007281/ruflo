// Package security 在系统边界提供输入校验、路径约束与安全执行等能力，采用防御式编程：
// 不信任外部字符串与路径，在进入业务逻辑前完成消毒、规范化与策略判定，降低注入、路径穿越与命令执行风险。
package security

// ValidationSeverity 表示单条校验结果的严重级别，用于聚合后决定是记录、告警还是阻断请求。
type ValidationSeverity string

const (
	SeverityInfo  ValidationSeverity = "info"  // 信息级，仅提示
	SeverityWarn  ValidationSeverity = "warn"  // 警告，可记录审计
	SeverityBlock ValidationSeverity = "block" // 阻断，整体校验视为不通过
)

// ValidationIssue 描述一次具体未通过的检查项（字段、原因、级别）。
type ValidationIssue struct {
	Field    string             `json:"field"`    // 出错字段或逻辑名
	Message  string             `json:"message"`  // 人类可读说明
	Severity ValidationSeverity `json:"severity"` // 该条问题的严重级别
}

// ValidationResult 聚合一次校验 pass 的全部问题；OK 为 true 当且仅当不存在 SeverityBlock。
type ValidationResult struct {
	OK     bool              `json:"ok"`               // 是否通过（无 block 级问题）
	Issues []ValidationIssue `json:"issues,omitempty"` // 发现的问题列表
}

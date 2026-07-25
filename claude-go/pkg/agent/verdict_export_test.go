package agent

// 标准 export_test 手法: 把包内符号暴露给**外部测试包** (package agent_test), 而不给
// 生产 API 增加一个只有测试用得上的导出函数。
// 消费方见 verdict_consistency_test.go。

// ExportedInferTurnVerdict 见 inferTurnVerdict。
func ExportedInferTurnVerdict(stopReason string, okTools, totalTools int) string {
	return string(inferTurnVerdict(stopReason, okTools, totalTools))
}

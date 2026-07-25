package builtin

import "github.com/anthropic/claude-go/pkg/evolution/replay"

// replayTask 造一个最小回放任务 (装配档测试用)。
// 单独一个文件是为了让 evotools_test.go 不必 import replay 只为一行构造。
func replayTask(objective string) replay.Task {
	return replay.Task{ID: "t", Objective: objective}
}

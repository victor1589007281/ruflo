// memprobe — 只读探针: 复现团队 stage 注入的 L1/L2 记忆块, 量它有多大。
// 只读 /tmp/memprobe/memory 的副本, 绝不碰真实 memory 目录。
package main

import (
	"fmt"
	"os"

	"github.com/anthropic/claude-go/pkg/memory"
)

const objective = `分析以下岗位 JD，为候选人产出面试备战包（需求画像 / 模拟面试题库 / 准备清单）。
jd_title: 高级 Go 存储研发工程师
company: 云锻科技
team_name: if-jd-analysis-e694e6e47758`

// main 复现团队 stage 注入的记忆块并打印各段字符数, 用于核对 memory_chars 口径。
//
// 注意：defer recover() 把 panic 静默吞掉 —— 探针崩了就只留半截输出, 退出码仍是 0。
func main() {
	defer func() { recover() }()
	dir := "/tmp/memprobe/memory"

	tiered := memory.NewTieredStoreWithPersist(dir)
	facts := memory.NewFactStore(dir)
	fmt.Printf("episodic=%d facts=%d\n", tiered.Count(), facts.Count())

	got := tiered.Retrieve(objective, 7)
	fmt.Printf("Retrieve(topK=7) 实际返回 %d 条\n", len(got))
	for i, e := range got {
		fmt.Printf("  [%d] len=%d topics=%v\n", i, len(e.Content), e.Topics)
	}
	relevant := memory.FormatForPrompt(got)
	_ = got
	factBlock := memory.FormatFactsForPrompt(facts.Retrieve(objective, 5))

	fmt.Printf("relevant_memories block = %d chars\n", len(relevant))
	fmt.Printf("structured_memories block = %d chars\n", len(factBlock))
	fmt.Printf("L1+L2 total = %d chars\n", len(relevant)+len(factBlock))

	consolidated, _ := os.ReadFile(dir + "/consolidated.md")
	// loadDreamMemories: 超 25000 才截断
	lt := len(consolidated)
	if lt > 25000 {
		lt = 25000 + len("\n...(truncated)")
	}
	wrap := len("<long_term_memory>\n") + len("The following is consolidated knowledge from past sessions:\n\n") + len("</long_term_memory>\n\n")
	fmt.Printf("long_term_memory block = %d chars (body %d + wrapper %d)\n", lt+wrap, lt, wrap)
	fmt.Printf("=> memory_chars 预期 ≈ %d\n", len(relevant)+len(factBlock)+lt+wrap)
}

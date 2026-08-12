// evo_distill_cmd.go —— evo distill 子命令 (手册 13.3.4 数据通道 T1/T5/T6 的轻量落地)。
//
// 从 Claude Code 会话转录 (~/.claude/projects/**/*.jsonl) 离线挖掘三类 harness 资产,
// 全程确定性解析、零 LLM 调用 (13.3.1 离线优先原则):
//
//	T1 few-shot 示例库 fewshot.jsonl: 任务首句 → 前 3 个工具调用序列 (仅成功会话)
//	T5 工具分布    tool_stats.json:  工具调用频次 (描述改写/掩码配额的实证依据)
//	T6 修复模式库  repair.jsonl:     is_error tool_result → 下一个成功 tool_use 的
//	                                (错误类型→恢复动作) 模式对
//
// 产出喂给: --ref 参照文件 (gepa-step/gepa-loop)、经验卡种子、CLAUDE.md 打磨依据。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// distillRec 一条转录行的最小解析面 (只取我们关心的字段)。
type distillRec struct {
	Type    string `json:"type"`
	Message struct {
			Content json.RawMessage `json:"content"`
	} `json:"message"`
	IsMeta bool `json:"isMeta"`
}

type distillBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`          // tool_use id
	Name      string          `json:"name"`        // tool_use 工具名
	ToolUseID string          `json:"tool_use_id"` // tool_result 回指
	IsError   bool            `json:"is_error"`    // tool_result
	Text      string          `json:"text"`        // text
	Content   json.RawMessage `json:"content"`     // tool_result 正文 (string 或 blocks)
}

// blockText 取 tool_result 正文文本 (string 或 [{type:text,text}] 两种形态)。
func blockText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &arr) == nil {
		var b strings.Builder
		for _, a := range arr {
			b.WriteString(a.Text)
		}
		return b.String()
	}
	return ""
}

func distillBlocks(raw json.RawMessage) []distillBlock {
	var arr []distillBlock
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return []distillBlock{{Type: "text", Text: s}}
	}
	return nil
}

func evoDistillCmd() *cobra.Command {
	var projectsDir, outDir string
	c := &cobra.Command{
		Use:   "distill",
		Short: "从 Claude Code 转录挖掘 T1 示例/T5 工具分布/T6 修复模式 (零 LLM)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(outDir) == "" {
				outDir = filepath.Join(os.Getenv("HOME"), ".claude-go", "evolution", "distill")
			}
			var files []string
			root := filepath.Clean(projectsDir)
			_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() && strings.HasSuffix(path, ".jsonl") {
					files = append(files, path)
				}
				return nil
			})
			if len(files) == 0 {
				return fmt.Errorf("%s 下无 jsonl 转录", root)
			}

			toolCounts := map[string]int{}
			type repairKey struct{ Tool, ErrKind, Recovery string }
			repairCounts := map[repairKey]int{}
			var fewshots []string

			for _, fp := range files {
				data, err := os.ReadFile(fp)
				if err != nil {
					continue
				}
				var (
					firstTask   string
					firstTools  []string
					pendingErr  string // 最近一条 is_error 的工具名+错误类型
					sessionErrs int
					sessionOK   = true
					idToName    = map[string]string{} // tool_use_id → 工具名 (tool_result 不带名字, 须回填)
				)
				for _, line := range strings.Split(string(data), "\n") {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					var rec distillRec
					if json.Unmarshal([]byte(line), &rec) != nil {
						continue
					}
					for _, blk := range distillBlocks(rec.Message.Content) {
						switch blk.Type {
						case "text":
							if rec.Type == "user" && firstTask == "" && !rec.IsMeta {
								t := strings.TrimSpace(blk.Text)
								if t != "" && !strings.HasPrefix(t, "<") { // 滤掉 local-command 等元信息
									firstTask = t
								}
							}
						case "tool_use":
							toolCounts[blk.Name]++
							if blk.ID != "" && blk.Name != "" {
								idToName[blk.ID] = blk.Name
							}
							if len(firstTools) < 3 {
								firstTools = append(firstTools, blk.Name)
							}
							if pendingErr != "" {
								// T6: 错误后的第一个 tool_use 即恢复动作
								parts := strings.SplitN(pendingErr, "|", 2)
								repairCounts[repairKey{parts[0], parts[1], blk.Name}]++
								pendingErr = ""
							}
						case "tool_result":
							if blk.IsError {
								sessionErrs++
								kind := "generic"
								// 错误归类 (轻量关键词, 不追求完备)
								low := strings.ToLower(blockText(blk.Content))
								switch {
								case strings.Contains(low, "not found") || strings.Contains(low, "no such file"):
									kind = "not_found"
								case strings.Contains(low, "permission"):
									kind = "permission"
								case strings.Contains(low, "未找到") || strings.Contains(low, "未命中"):
									kind = "not_found"
								case strings.Contains(low, "语法") || strings.Contains(low, "syntax"):
									kind = "syntax"
								}
								toolName := idToName[blk.ToolUseID]
								if toolName == "" {
									toolName = "?"
								}
								pendingErr = toolName + "|" + kind
								if sessionErrs > 8 { // 错误缠身的会话不产示例
									sessionOK = false
								}
							}
						}
					}
				}
				// T1: 只收"有工具使用且错误少"的会话
				if sessionOK && firstTask != "" && len(firstTools) >= 2 {
					task := firstTask
					if len(task) > 160 {
						task = task[:160] + "…"
					}
					rec := map[string]interface{}{
						"task":     task,
						"tool_seq": firstTools,
						"source":   filepath.Base(fp),
					}
					if b, err := json.Marshal(rec); err == nil {
						fewshots = append(fewshots, string(b))
					}
				}
			}

			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return err
			}
			// tool_stats.json
			type kv struct {
				K string `json:"tool"`
				V int    `json:"calls"`
			}
			var stats []kv
			for k, v := range toolCounts {
				stats = append(stats, kv{k, v})
			}
			sort.Slice(stats, func(i, j int) bool { return stats[i].V > stats[j].V })
			b, _ := json.MarshalIndent(stats, "", "  ")
			if err := os.WriteFile(filepath.Join(outDir, "tool_stats.json"), b, 0o644); err != nil {
				return err
			}
			// fewshot.jsonl
			if err := os.WriteFile(filepath.Join(outDir, "fewshot.jsonl"),
				[]byte(strings.Join(fewshots, "\n")+"\n"), 0o644); err != nil {
				return err
			}
			// repair.jsonl (按频次排序取 top 200)
			type repairRow struct {
				Tool, ErrKind, Recovery string
				Count                   int
			}
			var rows []repairRow
			for k, v := range repairCounts {
				rows = append(rows, repairRow{k.Tool, k.ErrKind, k.Recovery, v})
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].Count > rows[j].Count })
			var rb strings.Builder
			for i, r := range rows {
				if i >= 200 {
					break
				}
				b, _ := json.Marshal(r)
				rb.Write(b)
				rb.WriteByte('\n')
			}
			if err := os.WriteFile(filepath.Join(outDir, "repair.jsonl"), []byte(rb.String()), 0o644); err != nil {
				return err
			}

			fmt.Printf("distill 完成: 转录 %d 个 → fewshot %d 条 / 修复模式 %d 类 / 工具 %d 种\n  产物: %s{fewshot.jsonl, repair.jsonl, tool_stats.json}\n",
				len(files), len(fewshots), len(rows), len(toolCounts), outDir+string(filepath.Separator))
			return nil
		},
	}
	c.Flags().StringVar(&projectsDir, "projects", filepath.Join(os.Getenv("HOME"), ".claude", "projects"), "Claude Code 转录目录")
	c.Flags().StringVar(&outDir, "out", "", "产物目录 (默认 <state>/evolution/distill/)")
	return c
}

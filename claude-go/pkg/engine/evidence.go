// evidence.go —— evidence.md 证据表的机械修补。
//
// 提示词已经禁止在「来源 URL」列写"同上", 但实测仍有 2/9 的任务写成
// `同上 E1 URL` (台积电 18 行、黄金 15 行)。这类指代在表格里读得通, 一旦
// 证据行被单独复制、被脚本解析、或交给下游核验就失效了 —— 而它并不是
// 内容错误, 只是一次纯机械的省略。
//
// 所以这里只做一件事: 在同一列内把"同上"展开成上一条真实 URL。不推断、
// 不改结论、不碰其他列; 没有可展开的目标时原样保留。
package engine

import (
	"os"
	"strings"

	"github.com/anthropic/claude-go/pkg/logging"
)

// normalizeEvidenceFile 展开 evidence.md 表格 URL 列里的"同上"。
//
// 返回展开的行数。文件不存在、不是表格、或没有可展开项时静默返回 0 ——
// 这是交付物的可选修补, 失败不应该影响 run 的结果。
func normalizeEvidenceFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	out, changed := expandSameAsAbove(string(data))
	if changed == 0 {
		return 0
	}
	if err := writeFileAtomic(path, []byte(out)); err != nil {
		return 0
	}
	logging.For("engine").Info("evidence.md 的「同上」引用已展开", "rows", changed)
	return changed
}

// expandSameAsAbove 在每一张表内, 把「来源 URL」列的"同上"展开成上一条完整 URL。
//
// 为什么也用在最终报告上: 提示词明确禁止证据表写"同上", 实测 vdb 仍有 7 行
// 这么写。这些行单独复制出来就失去来源, 而展开是纯机械的, 不需要模型重写。
//
// 关键约束:
//   - 只动**表头含 URL 的那一列**; "时间"列的"同上"是合法写法, 不能改;
//   - URL 只在**同一张表内**向后传递: 换表(遇到非表格行)就重置, 否则第二张
//     表的第一行会捡到上一张表最后一条 URL, 这是凭空造引用;
//   - 没有可展开目标时原样返回, 不新增/删除行。
func expandSameAsAbove(text string) (string, int) {
	lines := strings.Split(text, "\n")
	urlCol := -1
	lastURL := ""
	inTable := false
	changed := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			inTable = false
			urlCol = -1
			lastURL = ""
			continue
		}
		cells := strings.Split(trimmed, "|")
		// 形如 | a | b | 的行 split 后首尾各有一个空串。
		if len(cells) < 3 {
			continue
		}
		if !inTable {
			inTable = true
			urlCol = -1
			lastURL = ""
			for j, c := range cells {
				if strings.Contains(strings.ToUpper(c), "URL") {
					urlCol = j
					break
				}
			}
			continue
		}
		if urlCol < 0 || urlCol >= len(cells) || tableSeparatorRE.MatchString(trimmed) {
			continue
		}
		cell := strings.TrimSpace(cells[urlCol])
		if m := citationURLRE.FindString(cell); m != "" {
			lastURL = normalizeCitationURL(m)
			continue
		}
		if lastURL == "" || !strings.Contains(cell, "同上") {
			continue
		}
		cells[urlCol] = " " + lastURL + " "
		lines[i] = strings.Join(cells, "|")
		changed++
	}
	return strings.Join(lines, "\n"), changed
}

// backfillEvidenceFile 在 evidence.md 缺失或为空时, 用最终报告里的证据表回填。
//
// 为什么值得做: 调研角色的提示词要求"每拿到一条证据就落账", 但实测仍有任务
// 交回一份 0 字节的 evidence.md (cmb)。报告本身有 20 行证据表 —— 数据没丢,
// 只是没有落到交付物里; 下游拿 evidence.md 做核验时看到的是空文件。
//
// 回填严格机械: 只抄报告里已有的表格块, 不新增行、不改数字。文件非空时
// 一律不动 (那是模型自己写的, 可能比报告表格更细); 报告里没有表格时只告警。
func backfillEvidenceFile(path, report string) bool {
	if strings.TrimSpace(report) == "" {
		return false
	}
	if data, err := os.ReadFile(path); err == nil {
		if strings.TrimSpace(string(data)) != "" {
			return false
		}
		logging.For("engine").Warn("evidence.md 为空, 尝试用报告证据表回填", "path", path)
	} else if os.IsNotExist(err) {
		logging.For("engine").Warn("evidence.md 未产出, 尝试用报告证据表回填", "path", path)
	} else {
		logging.For("engine").Warn("evidence.md 读取失败, 跳过回填", "path", path, "err", err)
		return false
	}
	table := extractEvidenceTable(report)
	if table == "" {
		logging.For("engine").Warn("evidence.md 缺失或为空, 且报告中找不到可回填的证据表", "path", path)
		return false
	}
	content := "# 证据表\n\n> 本文件在运行结束时缺失或为空, 下表由引擎从最终报告机械回填, 未新增任何事实。\n\n" + table + "\n"
	if err := writeFileAtomic(path, []byte(content)); err != nil {
		logging.For("engine").Warn("evidence.md 回填失败", "path", path, "err", err)
		return false
	}
	logging.For("engine").Info("evidence.md 已用报告证据表回填", "path", path, "rows", strings.Count(table, "\n")+1)
	return true
}

// extractEvidenceTable 从报告里挑出证据表块 (连续以 `|` 开头的行)。
//
// 报告里可能有多张表 (对比表/时间线)。判据优先看表头是否含"来源 / URL /
// 摘录 / 证据", 再看块内 URL 行数 —— 证据表的特征是每行都带 URL。
func extractEvidenceTable(report string) string {
	var blocks [][]string
	var cur []string
	for _, line := range strings.Split(report, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "|") {
			cur = append(cur, s)
			continue
		}
		if len(cur) > 0 {
			blocks = append(blocks, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		blocks = append(blocks, cur)
	}
	best := ""
	bestScore := -1
	for _, b := range blocks {
		// 至少要有表头 + 分隔行 + 1 条数据行。
		if len(b) < 3 {
			continue
		}
		header := strings.ToLower(b[0])
		score := 0
		for _, kw := range []string{"来源", "url", "摘录", "证据", "source"} {
			if strings.Contains(header, kw) {
				score += 10
			}
		}
		urlRows := 0
		for _, row := range b {
			if citationURLRE.MatchString(row) {
				urlRows++
			}
		}
		if urlRows == 0 {
			continue
		}
		score += urlRows
		if score > bestScore {
			bestScore = score
			best = strings.Join(b, "\n")
		}
	}
	return best
}

// Package guard 提供排障执行器的只读守卫。
//
// SQL 守卫的目标不是“把语法解析全”，而是把**任何可能改变目标库状态或长占资源**
// 的语句挡在执行器之前。执行器本身还会用只读账号、只读事务和超时兜底；
// 三层同时存在是刻意的：守卫拦明显意图，账号拦漏网语句，超时拦慢查询。
package guard

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxStatementBytes 单条 SQL 的长度上限（防止把大文件塞进查询）。
const MaxStatementBytes = 16 * 1024

var (
	// 只允许这些首关键字。EXPLAIN 在本包里只接受“解释查询计划”，
	// EXPLAIN ANALYZE 会真正执行，见 ValidateReadOnly 的单独拦截。
	allowedFirst = map[string]bool{
		"SELECT": true, "SHOW": true, "EXPLAIN": true,
		"DESC": true, "DESCRIBE": true, "WITH": true,
	}

	// 出现在 SELECT/EXPLAIN/WITH 中即拒绝的关键字（按词边界匹配）。
	bannedWords = []string{
		"INSERT", "UPDATE", "DELETE", "REPLACE", "MERGE",
		"DROP", "ALTER", "CREATE", "TRUNCATE", "RENAME",
		"GRANT", "REVOKE", "SET", "KILL", "CALL", "USE",
		"LOAD", "OUTFILE", "DUMPFILE",
		"LOCK", "UNLOCK", "FLUSH", "RESET", "PURGE",
		"SHUTDOWN", "RESTART", "HANDLER", "PREPARE", "EXECUTE", "DEALLOCATE",
		"ANALYZE", "OPTIMIZE", "REPAIR", "CHECKSUM", "INSTALL", "UNINSTALL",
		"CHANGE", "PROMOTE", "CLONE",
	}

	forUpdateRe = regexp.MustCompile(`(?i)\bFOR\s+UPDATE\b`)
	shareLockRe = regexp.MustCompile(`(?i)\bLOCK\s+IN\s+SHARE\s+MODE\b`)
	intoFileRe  = regexp.MustCompile(`(?i)\bINTO\s+(OUTFILE|DUMPFILE)\b`)
)

// ValidateReadOnly 校验一条 SQL 是否只读、单语句、无副作用。
//
// 允许：SELECT / SHOW / EXPLAIN <query> / DESC / DESCRIBE / WITH ... SELECT。
// 拒绝：任何 DML/DDL/DCL、多语句、SELECT ... INTO OUTFILE、FOR UPDATE、
// LOCK IN SHARE MODE、EXPLAIN ANALYZE、SLEEP/BENCHMARK/GET_LOCK/LOAD_FILE。
func ValidateReadOnly(query string) error {
	q := strings.TrimSpace(query)
	if q == "" {
		return fmt.Errorf("SQL 为空")
	}
	if len(q) > MaxStatementBytes {
		return fmt.Errorf("SQL 超过 %d 字节", MaxStatementBytes)
	}
	if strings.Contains(q, "/*!") {
		return fmt.Errorf("拒绝 MySQL 可执行注释 /*! ... */")
	}

	stripped, err := stripLiteralsAndComments(q)
	if err != nil {
		return err
	}
	stripped = strings.TrimSpace(stripped)
	if stripped == "" {
		return fmt.Errorf("SQL 去掉注释后为空")
	}
	if strings.Contains(stripped, ";") {
		return fmt.Errorf("只允许单条语句（发现分号）")
	}

	first := strings.ToUpper(firstWord(stripped))
	if !allowedFirst[first] {
		return fmt.Errorf("只允许 SELECT/SHOW/EXPLAIN/DESC/DESCRIBE/WITH，实际首关键字 %q", first)
	}
	if intoFileRe.MatchString(stripped) {
		return fmt.Errorf("拒绝 SELECT ... INTO OUTFILE/DUMPFILE")
	}
	if forUpdateRe.MatchString(stripped) {
		return fmt.Errorf("拒绝 FOR UPDATE（会加锁）")
	}
	if shareLockRe.MatchString(stripped) {
		return fmt.Errorf("拒绝 LOCK IN SHARE MODE（会加锁）")
	}
	if strings.Contains(strings.ToUpper(stripped), "SLEEP(") ||
		strings.Contains(strings.ToUpper(stripped), "BENCHMARK(") ||
		strings.Contains(strings.ToUpper(stripped), "GET_LOCK(") ||
		strings.Contains(strings.ToUpper(stripped), "LOAD_FILE(") {
		return fmt.Errorf("拒绝可能阻塞或读取宿主文件的函数")
	}

	switch first {
	case "SHOW", "DESC", "DESCRIBE":
		// SHOW CREATE TABLE / SHOW GRANTS 等是只读的，不能套用 SELECT 的禁词表
		// （否则 CREATE 会被误杀）。它们仍受上面的多语句/文件/锁检查约束。
		return nil
	case "EXPLAIN":
		upper := strings.ToUpper(stripped)
		if strings.HasPrefix(upper, "EXPLAIN ANALYZE") || strings.Contains(upper, " EXPLAIN ANALYZE ") {
			return fmt.Errorf("拒绝 EXPLAIN ANALYZE（会实际执行查询）")
		}
		return checkBannedWords(stripped)
	default:
		return checkBannedWords(stripped)
	}
}

// checkBannedWords 按词边界检查禁词。
func checkBannedWords(stripped string) error {
	upper := strings.ToUpper(stripped)
	for _, w := range bannedWords {
		re := regexp.MustCompile(`\b` + w + `\b`)
		if re.MatchString(upper) {
			return fmt.Errorf("只读守卫拒绝关键字 %s", w)
		}
	}
	return nil
}

// firstWord 返回第一个字母/数字/下划线组成的词。
func firstWord(s string) string {
	for i, r := range s {
		if isWordRune(r) {
			j := i
			for j < len(s) && isWordRune(rune(s[j])) {
				j++
			}
			return s[i:j]
		}
	}
	return ""
}

func isWordRune(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// stripLiteralsAndComments 把字符串字面量、标识符引号和注释替换成空格，
// 只留下会被 SQL 引擎解释为语法/关键字的文本。
//
// 这不是完整 SQL 解析器；它只服务于“禁词不能被引号/注释藏起来”这一个目标。
// 未闭合的引号/注释按错误处理 —— 与其猜，不如拒绝。
func stripLiteralsAndComments(q string) (string, error) {
	var b strings.Builder
	b.Grow(len(q))
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			quote := c
			i++
			closed := false
			for i < len(q) {
				if q[i] == '\\' && quote != '`' {
					i += 2
					continue
				}
				if q[i] == quote {
					if i+1 < len(q) && q[i+1] == quote { // '' 转义
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return "", fmt.Errorf("SQL 字符串/标识符引号未闭合")
			}
			b.WriteByte(' ')
		case c == '-' && i+1 < len(q) && q[i+1] == '-' &&
			(i+2 == len(q) || q[i+2] == ' ' || q[i+2] == '\t' || q[i+2] == '\n' || q[i+2] == '\r'):
			for i < len(q) && q[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case c == '#':
			for i < len(q) && q[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			end := strings.Index(q[i+2:], "*/")
			if end < 0 {
				return "", fmt.Errorf("SQL 块注释未闭合")
			}
			i += 2 + end + 2
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

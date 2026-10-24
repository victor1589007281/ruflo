package dashboard

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// strconvItoa 把字符串解析成 int (strconv.Atoi 的薄包装)。
func strconvItoa(s string) (int, error) {
	return strconv.Atoi(s)
}

// strconvParseFloat 把字符串解析成 float64 (strconv.ParseFloat 的薄包装)。
func strconvParseFloat(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

// defaultStr 在 s 为空串时返回默认值 d, 否则返回 s。
func defaultStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// safeName 判定名字可否安全用作路径片段: 非空、不含 ".."、不含路径分隔符。
//
// 这是**路径穿越的唯一一道闸**, 但它只按字符串判, 不解析符号链接。
func safeName(name string) bool {
	if name == "" {
		return false
	}
	if strings.Contains(name, "..") {
		return false
	}
	if strings.ContainsAny(name, "/\\") {
		return false
	}
	return true
}

// splitLines 按 '\n' 切分字节流; 空行被丢弃, 末尾无换行时最后一段也算一行。
//
// 返回的子切片**共享**入参底层数组 (无拷贝) —— 调用方若长期持有需自己复制。
func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				lines = append(lines, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

// parseDurationSec 支持 Go time.ParseDuration (如 "1m23s") 以及纯数字秒。
func parseDurationSec(s string) float64 {
	if s == "" {
		return 0
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d.Seconds()
	}
	return 0
}

// sprintf 是 fmt.Sprintf 的薄包装 (供模板里当函数值用)。
func sprintf(format string, a ...interface{}) string {
	return fmt.Sprintf(format, a...)
}

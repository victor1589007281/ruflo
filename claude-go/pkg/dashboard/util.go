package dashboard

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

func strconvItoa(s string) (int, error) {
	return strconv.Atoi(s)
}

func strconvParseFloat(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

func defaultStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

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

func sprintf(format string, a ...interface{}) string {
	return fmt.Sprintf(format, a...)
}

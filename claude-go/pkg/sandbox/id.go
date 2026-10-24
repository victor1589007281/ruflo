package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// newID 生成沙箱运行 ID: "sbx_<UTC 时间戳>_<6 字节随机十六进制>"; 随机源失败时退化为纳秒时间戳。
func newID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "sbx_" + time.Now().UTC().Format("20060102T150405") + "_" + hex.EncodeToString(b[:])
	}
	return "sbx_" + time.Now().UTC().Format("20060102T150405.000000000")
}

package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

func newID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "sbx_" + time.Now().UTC().Format("20060102T150405") + "_" + hex.EncodeToString(b[:])
	}
	return "sbx_" + time.Now().UTC().Format("20060102T150405.000000000")
}

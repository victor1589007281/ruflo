package consensus

import (
	"crypto/rand"
	"encoding/hex"
)

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

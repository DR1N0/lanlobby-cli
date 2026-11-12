// Package local binds a foreground membership to owner-only local IPC.
package local

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

var ErrJoined = errors.New("this room/user already has an active local join")

func key(room, user string) string {
	b, _ := json.Marshal([2]string{room, user})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:16])
}

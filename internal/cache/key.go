package cache

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// keyVersion is bumped whenever the prompt or result format changes, so old
// entries stop matching.
const keyVersion = "v2"

// GenerateKey creates a SHA-256 cache key from every input that affects the
// answer: model, mode, command, man section, question and OS.
func GenerateKey(parts ...string) string {
	data := keyVersion + "\x00" + strings.Join(parts, "\x00")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(data)))
}

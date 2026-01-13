package cache

import (
	"crypto/sha256"
	"fmt"
)

// GenerateKey creates a SHA-256 hash key for caching
// Key is based on: command + question + model_id
func GenerateKey(command, question, model string) string {
	data := fmt.Sprintf("%s:%s:%s", command, question, model)
	hash := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", hash)
}

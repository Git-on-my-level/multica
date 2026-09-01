package util

import "fmt"

const sha256ClientKeyPrefix = "sha256:"

// ParseSHA256ClientKey validates the public idempotency-key representation and
// returns the lowercase hexadecimal digest stored by the authority. Keeping the
// accepted form deliberately narrow makes retries portable across API and CLI
// clients and prevents accidentally persisting a user-supplied brief as a key.
func ParseSHA256ClientKey(key string) (string, error) {
	if len(key) != len(sha256ClientKeyPrefix)+64 || key[:len(sha256ClientKeyPrefix)] != sha256ClientKeyPrefix {
		return "", fmt.Errorf("client key must use sha256:<64 lowercase hex> format")
	}
	digest := key[len(sha256ClientKeyPrefix):]
	for _, ch := range digest {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return "", fmt.Errorf("client key must use sha256:<64 lowercase hex> format")
		}
	}
	return digest, nil
}

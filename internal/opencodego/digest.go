package opencodego

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"sync"
)

// digestKey is a random per-process HMAC key. Digests derived from it are
// in-memory identity keys only: they cannot verify a credential and are
// useless across restarts.
var (
	digestKey  [32]byte
	digestOnce sync.Once
)

func initDigestKey() {
	if _, err := rand.Read(digestKey[:]); err != nil {
		panic("opencodego: crypto/rand unavailable: " + err.Error())
	}
}

// credentialDigest identifies an API key for in-memory duplicate detection
// (auth.go) and catalog cache keys (models.go). The per-process HMAC key
// means the digest is not a password hash and can never be used to verify or
// recover the key outside this process.
func credentialDigest(key string) [32]byte {
	digestOnce.Do(initDigestKey)
	mac := hmac.New(sha256.New, digestKey[:])
	_, _ = mac.Write([]byte(key))
	var digest [32]byte
	copy(digest[:], mac.Sum(nil))
	return digest
}

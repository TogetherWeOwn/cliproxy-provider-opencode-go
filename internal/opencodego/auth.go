package opencodego

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"sync"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func newAuth() kit.APIKeyAuth {
	return kit.APIKeyAuth{Provider: "opencode-go", Validate: func(fields map[string]any) error {
		for _, name := range []string{"quota_api_key", "workspace"} {
			if v, ok := fields[name]; ok {
				if _, ok := v.(string); !ok {
					return &pluginabi.Error{Code: "invalid_auth", Message: "invalid provider auth field", HTTPStatus: 400}
				}
			}
		}
		return nil
	}}
}

// proxyAuth records the host's global proxy-url from every auth parse and
// refresh. An auth is parsed before it can execute, so catalog discovery on
// the executor path has the host proxy even before any ModelsForAuth call.
//
// It also guards against the same API key being configured in two auth
// files: ownership is by filename, so a watcher's repeated parse of one file
// is safe. Only HMAC-SHA256 digests under a random per-process key are
// retained; keys never appear in duplicate errors and digests are useless
// across restarts.
type proxyAuth struct {
	kit.APIKeyAuth
	catalog *catalog
	mu      sync.Mutex
	macKey  [32]byte
	files   map[string][32]byte
	owners  map[[32]byte]string
}

func newProxyAuth(catalog *catalog) *proxyAuth {
	a := &proxyAuth{APIKeyAuth: newAuth(), catalog: catalog, files: make(map[string][32]byte), owners: make(map[[32]byte]string)}
	if _, err := rand.Read(a.macKey[:]); err != nil {
		panic("proxyAuth: crypto/rand unavailable: " + err.Error())
	}
	return a
}

// keyDigest identifies a key for duplicate detection only. The per-process
// HMAC key means the digest is not a password hash and cannot be verified
// or reused outside this process.
func (a *proxyAuth) keyDigest(key string) [32]byte {
	mac := hmac.New(sha256.New, a.macKey[:])
	_, _ = mac.Write([]byte(key))
	var digest [32]byte
	copy(digest[:], mac.Sum(nil))
	return digest
}

func (a *proxyAuth) ParseAuth(ctx context.Context, r pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	a.catalog.setHostProxy(r.Host.ProxyURL)
	out, err := a.APIKeyAuth.ParseAuth(ctx, r)
	if err != nil || !out.Handled {
		return out, err
	}
	key, err := kit.APIKey(out.Auth.StorageJSON, out.Auth.Metadata)
	if err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	digest := a.keyDigest(key)
	a.mu.Lock()
	defer a.mu.Unlock()
	if file, ok := a.owners[digest]; ok && file != r.FileName {
		return pluginapi.AuthParseResponse{}, &pluginabi.Error{Code: "duplicate_auth", Message: "the same API key is already configured in another auth file", HTTPStatus: 400}
	}
	if old, ok := a.files[r.FileName]; ok && old != digest {
		delete(a.owners, old)
	}
	a.files[r.FileName] = digest
	a.owners[digest] = r.FileName
	return out, nil
}
func (a *proxyAuth) RefreshAuth(ctx context.Context, r pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	a.catalog.setHostProxy(r.Host.ProxyURL)
	return a.APIKeyAuth.RefreshAuth(ctx, r)
}

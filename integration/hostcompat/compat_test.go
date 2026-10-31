//go:build cgo && (linux || darwin || freebsd)

package hostcompat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The host's actual loader and auth adapter must accept the experimental
// artifact. This deliberately does not claim inference or quota compatibility.
func TestRealHostLoaderAuthOnly(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	arch := filepath.Join(dir, runtime.GOOS, runtime.GOARCH)
	if err = os.MkdirAll(arch, 0755); err != nil {
		t.Fatal(err)
	}
	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	}
	build := exec.Command("go", "build", "-buildmode=c-shared", "-o", filepath.Join(arch, "opencode-go"+ext), "./cmd/plugin")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	enabled := true
	cfg := &config.Config{AuthDir: t.TempDir(), Plugins: config.PluginsConfig{Enabled: true, Dir: dir, Configs: map[string]config.PluginInstanceConfig{"opencode-go": {Enabled: &enabled}}}}
	host := pluginhost.New()
	defer host.ShutdownAll()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	host.ApplyConfig(ctx, cfg)
	if !host.PluginLoaded("opencode-go") || !host.PluginRegistered("opencode-go") || !host.HasAuthProvider("opencode-go") {
		t.Fatalf("provider failed to register: %+v", host.RegisteredPlugins())
	}
	if host.HasQuotaProvider("opencode-go") {
		t.Fatal("unverified quota capability advertised")
	}
	auth, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "opencode-go", FileName: "example.json", RawJSON: []byte(`{"type":"opencode-go","api_key":"example-key"}`)})
	if err != nil || !handled || auth == nil || auth.Provider != "opencode-go" {
		t.Fatalf("auth adapter failed: handled=%v err=%v", handled, err)
	}
}

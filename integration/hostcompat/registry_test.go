package hostcompat

import (
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginstore"
)

func TestRegistryAcceptedByPinnedHostStore(t *testing.T) {
	raw, err := os.ReadFile("../../registry.json")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := pluginstore.ParseRegistry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Plugins) != 1 {
		t.Fatal("expected one provider plugin")
	}
	plugin := registry.Plugins[0]
	if plugin.ID != "opencode-go" || plugin.Version != "0.1.2" || pluginstore.PluginInstallType(plugin) != "github-release" {
		t.Fatal("wrong release identity")
	}
	name := pluginstore.ArchiveName(plugin.ID, plugin.Version, "linux", "amd64")
	if name != "opencode-go_0.1.2_linux_amd64.zip" {
		t.Fatalf("unexpected host asset %s", name)
	}
	_, _, err = pluginstore.SelectReleaseAssets(pluginstore.Release{TagName: "v0.1.2", Assets: []pluginstore.ReleaseAsset{{Name: name}, {Name: "checksums.txt"}}}, plugin.ID, plugin.Version, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
}

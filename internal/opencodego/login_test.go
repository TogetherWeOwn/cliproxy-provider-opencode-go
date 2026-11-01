package opencodego

import (
	"context"
	"encoding/json"
	"html"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	testResourceBase = "/v0/resource/plugins/opencode-go-id"
	testSubmitPath   = "/v0/management/plugin-login/opencode-go"
	testBaseURL      = "http://127.0.0.1:19000/v0/management/oauth-callback"
)

// TestAPIKeyLoginWired drives the management-UI login flow end to end through
// the provider's own ParseAuth pipeline: page, key+workspace submit, poll.
func TestAPIKeyLoginWired(t *testing.T) {
	ctx := context.Background()
	c := newCatalog("http://opencode.invalid", nil)
	defer c.client.CloseIdleConnections()
	p, err := newProxyAuth(c)
	if err != nil {
		t.Fatal(err)
	}
	login := p.Login
	if login == nil {
		t.Fatal("API-key login not wired")
	}
	reg, err := login.RegisterManagement(ctx, pluginapi.ManagementRegistrationRequest{
		BasePath: "/v0/management", ResourceBasePath: testResourceBase})
	if err != nil {
		t.Fatal(err)
	}
	var sawSubmit, sawPage bool
	for _, r := range reg.Routes {
		if r.Method == "POST" && r.Path == "plugin-login/opencode-go" {
			sawSubmit = true
		}
	}
	for _, r := range reg.Resources {
		if r.Path == "login" {
			sawPage = true
		}
	}
	if !sawSubmit || !sawPage {
		t.Fatalf("login routes not registered: %+v", reg)
	}
	start, err := p.StartLogin(ctx, pluginapi.AuthLoginStartRequest{Provider: "opencode-go",
		BaseURL: testBaseURL, Host: pluginapi.HostConfigSummary{AuthDir: "/tmp/opencode-go-auth"}})
	if err != nil {
		t.Fatal(err)
	}
	// The kit returns a root-relative login URL so the panel resolves it
	// against its own origin instead of the host's loopback callback base.
	if !strings.HasPrefix(start.URL, testResourceBase+"/login?state=") {
		t.Fatalf("unexpected login URL %q", start.URL)
	}
	poll, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || poll.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("fresh login not pending: %+v %v", poll, err)
	}
	page, err := login.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "GET",
		Path: testResourceBase + "/login", Query: map[string][]string{"state": {start.State}}})
	if err != nil || page.StatusCode != 200 {
		t.Fatalf("login page %+v %v", page, err)
	}
	body := html.UnescapeString(string(page.Body))
	// The quota key is a secret: hidden input, like the main API key. The
	// workspace stays visible.
	for _, want := range []string{"read -rs PLUGIN_API_KEY", "read -rs PLUGIN_FIELD_QUOTA_API_KEY", "read -r PLUGIN_FIELD_WORKSPACE", `"workspace":"$ESC_FIELD_WORKSPACE"`, "Workspace"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing login field wiring %q", want)
		}
	}
	if strings.Contains(body, "read -r PLUGIN_FIELD_QUOTA_API_KEY") {
		t.Fatal("quota key read with echoing input")
	}
	submit, err := login.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "POST",
		Path: testSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"example-key","workspace":"team-a","quota_api_key":""}`)})
	if err != nil || submit.StatusCode != 200 {
		t.Fatalf("submit %+v %v", submit, err)
	}
	done, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || done.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll %+v %v", done, err)
	}
	var store map[string]any
	if err := json.Unmarshal(done.Auth.StorageJSON, &store); err != nil {
		t.Fatal(err)
	}
	if done.Auth.Provider != "opencode-go" || store["workspace"] != "team-a" {
		t.Fatalf("workspace lost through login: %+v %s", done.Auth, done.Auth.StorageJSON)
	}
	if _, ok := store["quota_api_key"]; ok {
		t.Fatalf("blank optional field not skipped: %s", done.Auth.StorageJSON)
	}
	// A second poll must not redeliver credentials.
	again, err := p.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || again.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("consumed login redelivered: %+v %v", again, err)
	}
}

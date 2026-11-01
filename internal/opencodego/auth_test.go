package opencodego

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestAuthValidationAndStableIdentity(t *testing.T) {
	auth := newAuth()
	raw := []byte(`{"type":"opencode-go","api_key":"example-key","quota_api_key":"example-quota","workspace":"example","weight":2}`)
	a, err := auth.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: raw, FileName: "one.json"})
	if err != nil || !a.Handled || !strings.HasPrefix(a.Auth.ID, "opencode-go:") {
		t.Fatalf("parse %+v %v", a, err)
	}
	b, err := auth.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: raw, FileName: "two.json"})
	if err != nil || a.Auth.ID != b.Auth.ID {
		t.Fatalf("identity changed")
	}
	for _, field := range []string{`"quota_api_key":42`, `"workspace":false`} {
		_, err = auth.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"opencode-go","api_key":"example",` + field + `}`)})
		if err == nil {
			t.Errorf("invalid field accepted: %s", field)
		}
	}
}

func TestProxyAuthRejectsDuplicateKeyAcrossFiles(t *testing.T) {
	c := newCatalog("http://opencode.invalid", nil)
	defer c.client.CloseIdleConnections()
	auth, err := newProxyAuth(c)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"type":"opencode-go","api_key":"shared-key"}`)
	first, err := auth.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: raw, FileName: "one.json"})
	if err != nil || !first.Handled {
		t.Fatalf("first parse %+v %v", first, err)
	}
	// Re-parsing the same file (for example on a host watcher rescan) stays valid.
	if _, err := auth.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: raw, FileName: "one.json"}); err != nil {
		t.Fatalf("same-file re-parse rejected: %v", err)
	}
	// The same key in a different file is a duplicate account.
	_, err = auth.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: raw, FileName: "two.json"})
	var dup *pluginabi.Error
	if err == nil || !errors.As(err, &dup) || dup.Code != "duplicate_auth" {
		t.Fatalf("duplicate key accepted: %v", err)
	}
	// Rotating the key in the original file releases the old digest.
	rotated := []byte(`{"type":"opencode-go","api_key":"rotated-key"}`)
	if _, err := auth.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: rotated, FileName: "one.json"}); err != nil {
		t.Fatalf("rotation rejected: %v", err)
	}
	if _, err := auth.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: raw, FileName: "two.json"}); err != nil {
		t.Fatalf("released key still owned: %v", err)
	}
}

// TestAbandonedLoginReleasesKeyOwnership mirrors the kit's
// TestLoginAbandonedSubmitReleasesOwnership on this provider's wiring: a key
// submitted through the API-key login and then abandoned (never polled) is
// only reserved until the login session expires. After the TTL lapses the
// same key parses under a different filename instead of failing as
// duplicate_auth until plugin restart.
func TestAbandonedLoginReleasesKeyOwnership(t *testing.T) {
	ctx := context.Background()
	c := newCatalog("http://opencode.invalid", nil)
	defer c.client.CloseIdleConnections()
	p, err := newProxyAuthWithLoginTTL(c, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	login := p.Login
	if login == nil {
		t.Fatal("API-key login not wired")
	}
	if _, err := login.RegisterManagement(ctx, pluginapi.ManagementRegistrationRequest{
		BasePath: "/v0/management", ResourceBasePath: testResourceBase}); err != nil {
		t.Fatal(err)
	}
	start, err := p.StartLogin(ctx, pluginapi.AuthLoginStartRequest{Provider: "opencode-go",
		BaseURL: testBaseURL, Host: pluginapi.HostConfigSummary{AuthDir: "/tmp/opencode-go-auth"}})
	if err != nil {
		t.Fatal(err)
	}
	submit, err := login.HandleManagement(ctx, pluginapi.ManagementRequest{Method: "POST",
		Path: testSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"abandoned-key"}`)})
	if err != nil || submit.StatusCode != 200 {
		t.Fatalf("submit %+v %v", submit, err)
	}
	// Abandon the login: never poll it.
	raw := []byte(`{"type":"opencode-go","api_key":"abandoned-key"}`)
	// While the login is in flight, its reservation still blocks another file.
	_, err = p.ParseAuth(ctx, pluginapi.AuthParseRequest{RawJSON: raw, FileName: "upload.json"})
	var dup *pluginabi.Error
	if err == nil || !errors.As(err, &dup) || dup.Code != "duplicate_auth" {
		t.Fatalf("parse during login = %v, want duplicate_auth", err)
	}
	// Once the abandoned session expires, the same parse owns the key. The
	// kit guard expires reservations lazily against its own clock, so poll
	// the parse until the TTL has lapsed instead of asserting on one sleep.
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err = p.ParseAuth(ctx, pluginapi.AuthParseRequest{RawJSON: raw, FileName: "upload.json"})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("abandoned login still owns key after session expiry: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	// The successful host parse commits ownership for upload.json.
	_, err = p.ParseAuth(ctx, pluginapi.AuthParseRequest{RawJSON: raw, FileName: "other.json"})
	if err == nil || !errors.As(err, &dup) || dup.Code != "duplicate_auth" {
		t.Fatalf("parse after commit = %v, want duplicate_auth", err)
	}
}

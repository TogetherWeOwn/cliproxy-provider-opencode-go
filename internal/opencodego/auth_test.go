package opencodego

import (
	"context"
	"errors"
	"strings"
	"testing"

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

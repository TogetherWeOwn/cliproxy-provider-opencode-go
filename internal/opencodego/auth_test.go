package opencodego

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"strings"
	"testing"
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

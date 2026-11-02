package opencodego

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestRuntimeRegistersNativeProviderCapabilitiesWithoutScheduler(t *testing.T) {
	r, err := NewRuntime(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Shutdown(context.Background())
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(r.Call(context.Background(), "plugin.register", []byte(`{"schema_version":10}`)), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK {
		t.Fatalf("registration failed: %+v", envelope.Error)
	}
	var result struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"quota_provider", "executor", "model_provider", "model_registrar"} {
		if result.Capabilities[name] != true {
			t.Errorf("missing capability %s: %+v", name, result.Capabilities)
		}
	}
	// API-key only (TWO-1257): no AuthProvider and no ManagementAPI login
	// routes, so the panel shows no login card.
	for _, name := range []string{"auth_provider", "management_api", "scheduler"} {
		if result.Capabilities[name] == true {
			t.Errorf("unexpected capability %s: %+v", name, result.Capabilities)
		}
	}
}

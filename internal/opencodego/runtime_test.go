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
	if err := json.Unmarshal(r.Call(context.Background(), "plugin.register", []byte(`{"schema_version":9}`)), &envelope); err != nil {
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
	for _, name := range []string{"auth_provider", "quota_provider", "executor", "model_provider", "model_registrar"} {
		if result.Capabilities[name] != true {
			t.Errorf("missing capability %s: %+v", name, result.Capabilities)
		}
	}
	if result.Capabilities["scheduler"] == true {
		t.Fatal("native selection must not be replaced")
	}
}

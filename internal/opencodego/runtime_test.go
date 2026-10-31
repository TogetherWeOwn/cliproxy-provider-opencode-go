package opencodego

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestExperimentalRuntimeAdvertisesOnlyImplementedSafeCapabilities(t *testing.T) {
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
		Capabilities struct {
			Auth     bool `json:"auth_provider"`
			Quota    bool `json:"quota_provider"`
			Executor bool `json:"executor"`
			Models   bool `json:"model_provider"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Capabilities.Auth || result.Capabilities.Quota || result.Capabilities.Executor || result.Capabilities.Models {
		t.Fatalf("unexpected capabilities: %+v", result.Capabilities)
	}
}

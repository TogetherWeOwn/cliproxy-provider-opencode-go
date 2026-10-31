package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const exampleConfig = `openai-compatibility:
  - name: opencode-go
    disabled: true
    prefix: example-team
    priority: 2
    api-key-entries:
      - api-key: example-inference-a
        weight: 2
        proxy-url: http://proxy.example.invalid:8080
      - api-key: example-inference-b
plugins:
  configs:
    subscription-pool:
      providers:
        opencode:
          accounts:
            - name: Example A
              dashboard-api-key: example-quota-a
            - name: Example B
              dashboard-api-key: example-quota-b
`

func TestMigrationPairsKeysPrivatelyAndDoesNotAlterConfig(t *testing.T) {
	raw := []byte(exampleConfig)
	before := string(raw)
	out := filepath.Join(t.TempDir(), "auths")
	count, err := migrate(raw, out)
	if err != nil || count != 2 {
		t.Fatalf("migration count %d error %v", count, err)
	}
	if string(raw) != before {
		t.Fatal("input changed")
	}
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) != 2 {
		t.Fatal("files missing")
	}
	for _, entry := range entries {
		path := filepath.Join(out, entry.Name())
		stat, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if stat.Mode().Perm() != 0600 {
			t.Fatal("auth file not private")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var auth map[string]any
		if json.Unmarshal(data, &auth) != nil {
			t.Fatal("invalid auth file")
		}
		if auth["type"] != "opencode-go" || auth["disabled"] != true || auth["prefix"] != "example-team" || auth["priority"] != float64(2) {
			t.Fatal("routing fields lost")
		}
		if auth["api_key"] == "example-inference-a" {
			if auth["quota_api_key"] != "example-quota-a" || auth["weight"] != float64(2) || auth["proxy_url"] != "http://proxy.example.invalid:8080" {
				t.Fatal("wrong account A pairing")
			}
		} else if auth["api_key"] != "example-inference-b" || auth["quota_api_key"] != "example-quota-b" {
			t.Fatal("wrong account B pairing")
		}
	}
	if _, err = migrate(raw, out); err == nil {
		t.Fatal("existing private directory overwritten")
	}
}
func TestMigrationRejectsAmbiguousOrMalformedInputWithoutPartialOutput(t *testing.T) {
	for _, raw := range []string{
		"not: [yaml",
		strings.Replace(exampleConfig, "      - api-key: example-inference-b", "", 1),
		strings.Replace(exampleConfig, "example-inference-b", "example-inference-a", 1),
		strings.Replace(exampleConfig, "example-inference-a", "", 1),
		strings.Replace(exampleConfig, "weight: 2", "weight: -1", 1),
		strings.Replace(exampleConfig, "api-key-entries:", "keys: []\n    api-key-entries:", 1),
	} {
		out := filepath.Join(t.TempDir(), "auths")
		_, err := migrate([]byte(raw), out)
		if err == nil {
			t.Fatal("ambiguous migration accepted")
		}
		if strings.Contains(err.Error(), "example-inference") || strings.Contains(err.Error(), "example-quota") {
			t.Fatal("credential in error")
		}
		if _, err = os.Stat(out); !os.IsNotExist(err) {
			t.Fatal("partial output left behind")
		}
	}
}

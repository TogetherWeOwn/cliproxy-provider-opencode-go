// Command migrate reads a config copy and writes disabled, private auth files.
// It never rewrites config, contacts a provider, or modifies a running service.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type legacyKey struct {
	Key    string `yaml:"api-key"`
	Weight *int   `yaml:"weight"`
	Proxy  string `yaml:"proxy-url"`
}
type legacyEntry struct {
	Name     string      `yaml:"name"`
	Prefix   string      `yaml:"prefix"`
	Priority int         `yaml:"priority"`
	Keys     []legacyKey `yaml:"keys"`
	Entries  []legacyKey `yaml:"api-key-entries"`
}
type legacyAccount struct {
	Name     string `yaml:"name"`
	QuotaKey string `yaml:"dashboard-api-key"`
}
type legacyModule struct {
	QuotaKey string          `yaml:"dashboard-api-key"`
	Accounts []legacyAccount `yaml:"accounts"`
}
type legacyConfig struct {
	Compat  []legacyEntry `yaml:"openai-compatibility"`
	Plugins struct {
		Configs map[string]struct {
			Providers map[string]legacyModule `yaml:"providers"`
		} `yaml:"configs"`
	} `yaml:"plugins"`
}

func main() {
	config := flag.String("config", "", "path to a private CLIProxy config copy")
	out := flag.String("out", "", "new private directory for disabled auth files")
	flag.Parse()
	if *config == "" || *out == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "Usage: migrate -config <config-copy.yaml> -out <new-auth-directory>")
		os.Exit(2)
	}
	stat, err := os.Stat(*config)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 8<<20 {
		fmt.Fprintln(os.Stderr, "Cannot read config (regular file, maximum 8 MiB required).")
		os.Exit(1)
	}
	raw, err := os.ReadFile(*config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Cannot read config.")
		os.Exit(1)
	}
	count, err := migrate(raw, *out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Migration failed; check input structure, account order, and an unused output directory. No credentials are printed.")
		os.Exit(1)
	}
	fmt.Printf("Wrote %d disabled auth files (0600). Review pairing and validate in staging before enabling.\n", count)
}

func migrate(raw []byte, out string) (count int, err error) {
	invalid := errors.New("invalid or ambiguous OpenCode Go migration configuration")
	var cfg legacyConfig
	if len(raw) > 8<<20 || yaml.Unmarshal(raw, &cfg) != nil {
		return 0, invalid
	}
	var entry *legacyEntry
	for i := range cfg.Compat {
		if cfg.Compat[i].Name == "opencode-go" {
			if entry != nil {
				return 0, invalid
			}
			entry = &cfg.Compat[i]
		}
	}
	if entry == nil || entry.Keys != nil && entry.Entries != nil {
		return 0, invalid
	}
	keys := entry.Entries
	if keys == nil {
		keys = entry.Keys
	}
	if len(keys) == 0 {
		return 0, invalid
	}
	module := cfg.Plugins.Configs["subscription-pool"].Providers["opencode"]
	if len(module.Accounts) > 0 && len(module.Accounts) != len(keys) {
		return 0, invalid
	}
	type file struct {
		name string
		data []byte
	}
	files := make([]file, 0, len(keys))
	seen := map[string]bool{}
	for i, key := range keys {
		value := strings.TrimSpace(key.Key)
		if value == "" || strings.ContainsAny(value, "\r\n") || seen[value] || key.Weight != nil && (*key.Weight < 0 || *key.Weight > 1000000) {
			return 0, invalid
		}
		seen[value] = true
		digest := sha256.Sum256([]byte(value))
		id := hex.EncodeToString(digest[:6])
		auth := map[string]any{"type": "opencode-go", "api_key": value, "disabled": true, "priority": entry.Priority}
		if entry.Prefix != "" {
			auth["prefix"] = entry.Prefix
		}
		if key.Weight != nil {
			auth["weight"] = *key.Weight
		}
		if key.Proxy != "" {
			auth["proxy_url"] = key.Proxy
		}
		quota := module.QuotaKey
		if len(module.Accounts) > 0 {
			a := module.Accounts[i]
			if a.QuotaKey != "" {
				quota = a.QuotaKey
			}
			if a.Name != "" {
				auth["label"] = a.Name
			}
		}
		if quota != "" {
			auth["quota_api_key"] = strings.TrimSpace(quota)
		}
		data, err := json.MarshalIndent(auth, "", "  ")
		if err != nil {
			return 0, invalid
		}
		files = append(files, file{name: "opencode-go-" + id + ".json", data: append(data, '\n')})
	}
	// Mkdir must create a new directory. Refuse existing paths (including
	// symlinks), rather than following or overwriting private files.
	if out == "" || os.Mkdir(out, 0700) != nil {
		return 0, errors.New("output must be a new private directory")
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(out)
		}
	}()
	for _, file := range files {
		f, err := os.OpenFile(filepath.Join(out, file.name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return 0, errors.New("cannot create private auth output")
		}
		_, writeErr := f.Write(file.data)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			return 0, errors.New("cannot write private auth output")
		}
	}
	complete = true
	return len(files), nil
}

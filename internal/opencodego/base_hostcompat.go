//go:build hostcompat

package opencodego

import (
	"context"
	"errors"
	"net"
	"net/url"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// Only explicitly test-tagged binaries accept a loopback-only mock upstream.
// Release packaging never enables this tag.
var testBase atomic.Value

func selectedBase() string {
	if value := testBase.Load(); value != nil {
		return value.(string)
	}
	return "http://127.0.0.1:0"
}
func configureBase(_ context.Context, raw []byte) error {
	var cfg struct {
		Base string `yaml:"hostcompat_base"`
	}
	if yaml.Unmarshal(raw, &cfg) != nil {
		return errors.New("invalid test configuration")
	}
	u, err := url.Parse(cfg.Base)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		return errors.New("test upstream must be loopback")
	}
	testBase.Store(cfg.Base)
	return nil
}

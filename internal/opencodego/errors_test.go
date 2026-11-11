package opencodego

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestErrorClassification(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name                string
		status              int
		body, retry, code   string
		wantStatus, seconds int
		retryable, scoped   bool
	}{
		{"rolling exhaustion", 429, `{"error":{"type":"GoUsageLimitError","message":"private-value"},"metadata":{"limitName":"5 hour"}}`, "123", "quota_exhausted_5h", 429, 123, true, true},
		{"weekly exhaustion", 429, `{"error":{"type":"GoUsageLimitError"},"metadata":{"limitName":"weekly"}}`, "7200", "quota_exhausted_weekly", 429, 7200, true, true},
		{"monthly exhaustion date", 429, `{"error":{"type":"GoUsageLimitError"},"metadata":{"limitName":"monthly"}}`, now.Add(time.Hour).Format(http.TimeFormat), "quota_exhausted_monthly", 429, 3600, true, true},
		{"unknown exhaustion", 429, `{"error":{"type":"GoUsageLimitError"},"metadata":{"limitName":"private-value"}}`, "bad", "quota_exhausted", 429, 60, true, true},
		{"transient limit", 429, `{"error":{"type":"RateLimitError"}}`, "", "rate_limited", 429, 5, true, false},
		{"auth", 401, `{"error":{"type":"AuthError"}}`, "", "authentication_failed", 401, 0, false, false},
		{"entitlement", 403, `{"error":{"type":"EntitlementError"}}`, "", "entitlement_required", 403, 0, false, false},
		{"region", 403, `{"error":{"type":"RegionError"}}`, "", "region_restricted", 403, 0, false, false},
		{"policy", 403, `{"error":{"type":"DataPolicyError"}}`, "", "data_policy_restricted", 403, 0, false, false},
		{"legacy credits", 401, `{"error":{"type":"CreditsError"}}`, "", "entitlement_required", 403, 0, false, false},
		{"legacy monthly", 401, `{"error":{"type":"MonthlyLimitError"}}`, "", "entitlement_required", 403, 0, false, false},
		{"legacy user limit", 401, `{"error":{"type":"UserLimitError"}}`, "", "entitlement_required", 403, 0, false, false},
		{"unsupported model", 401, `{"error":{"type":"ModelError"}}`, "", "model_unavailable", 404, 0, false, false},
		{"cloudflare json", 403, `{"error_code":1010,"error_name":"browser_signature_banned","retryable":false}`, "", "upstream_access_blocked", 502, 5, true, false},
		{"cloudflare html", 403, `<html>error code: 1010</html>`, "", "upstream_access_blocked", 502, 5, true, false},
		{"upstream failure", 503, `private-value`, "15", "upstream_error", 503, 15, true, false},
		{"malformed auth", 401, `private-value`, "", "authentication_failed", 401, 0, false, false},
		{"generic forbidden", 403, `private-value`, "", "forbidden", 403, 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyAt(pluginapi.HTTPResponse{StatusCode: tt.status, Body: []byte(tt.body), Headers: http.Header{"Retry-After": {tt.retry}}}, now)
			var e *pluginabi.Error
			if !errors.As(err, &e) {
				t.Fatalf("not an RPC error: %v", err)
			}
			if e.Code != tt.code || e.HTTPStatus != tt.wantStatus || e.RetryAfterSeconds != tt.seconds || e.Retryable != tt.retryable || e.CredentialScoped != tt.scoped || e.IsCredentialScoped() != tt.scoped {
				t.Fatalf("got %+v (IsCredentialScoped=%v)", e, e.IsCredentialScoped())
			}
			// The kit runtime's MapError is the carrier for stream/error_detail
			// envelopes: the scoped pair must survive it unchanged.
			if mapped := kit.MapError(err); mapped.CredentialScoped != tt.scoped || mapped.Code != tt.code {
				t.Fatalf("MapError dropped scope/code: %+v", mapped)
			}
			if strings.Contains(e.Error(), "private-value") {
				t.Fatal("error leaked provider body")
			}
		})
	}
	if classifyAt(pluginapi.HTTPResponse{StatusCode: 200}, now) != nil {
		t.Fatal("success classified as error")
	}
}

// TestGoUsageLimitErrorResetRetryAfter proves the scoped branch prefers a
// reset carried by the upstream body over the header fallback, with exact
// arithmetic under a pinned now, and that a longer server Retry-After still
// wins (a known reset is never shortened).
func TestGoUsageLimitErrorResetRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC)
	body := `{"error":{"type":"GoUsageLimitError"},"metadata":{"limitName":"weekly","resets_at":"2026-11-11T02:59:37Z"}}`
	for _, tc := range []struct {
		name    string
		retry   string
		wantSec int
	}{
		{"reset from body, no header", "", 3577},
		{"shorter header yields to reset", "60", 3577},
		{"longer header wins, reset never shortens", "7200", 7200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyAt(pluginapi.HTTPResponse{StatusCode: 429, Body: []byte(body), Headers: http.Header{"Retry-After": {tc.retry}}}, now)
			var e *pluginabi.Error
			if !errors.As(err, &e) {
				t.Fatalf("not an RPC error: %v", err)
			}
			if !e.CredentialScoped || e.Code != "quota_exhausted_weekly" || e.RetryAfterSeconds != tc.wantSec {
				t.Fatalf("got %+v, want scoped quota_exhausted_weekly with %ds", e, tc.wantSec)
			}
		})
	}
}

// TestGoUsageLimitErrorMessageReset proves a "resets at <RFC3339>" message in
// the upstream body also yields the exact reset-derived retry-after.
func TestGoUsageLimitErrorMessageReset(t *testing.T) {
	now := time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC)
	body := `{"error":{"type":"GoUsageLimitError","message":"usage limit reached. Your usage window resets at 2026-11-11T02:59:37Z."},"metadata":{"limitName":"monthly"}}`
	err := classifyAt(pluginapi.HTTPResponse{StatusCode: 429, Body: []byte(body)}, now)
	var e *pluginabi.Error
	if !errors.As(err, &e) {
		t.Fatalf("not an RPC error: %v", err)
	}
	if !e.CredentialScoped || e.RetryAfterSeconds != 3577 {
		t.Fatalf("got %+v, want credential-scoped with 3577s reset-derived retry-after", e)
	}
}

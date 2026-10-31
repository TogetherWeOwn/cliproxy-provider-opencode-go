package opencodego

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

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
		retryable           bool
	}{
		{"rolling exhaustion", 429, `{"error":{"type":"GoUsageLimitError","message":"private-value"},"metadata":{"limitName":"5 hour"}}`, "123", "quota_exhausted_5h", 429, 123, true},
		{"weekly exhaustion", 429, `{"error":{"type":"GoUsageLimitError"},"metadata":{"limitName":"weekly"}}`, "7200", "quota_exhausted_weekly", 429, 7200, true},
		{"monthly exhaustion date", 429, `{"error":{"type":"GoUsageLimitError"},"metadata":{"limitName":"monthly"}}`, now.Add(time.Hour).Format(http.TimeFormat), "quota_exhausted_monthly", 429, 3600, true},
		{"unknown exhaustion", 429, `{"error":{"type":"GoUsageLimitError"},"metadata":{"limitName":"private-value"}}`, "bad", "quota_exhausted", 429, 60, true},
		{"transient limit", 429, `{"error":{"type":"RateLimitError"}}`, "", "rate_limited", 429, 5, true},
		{"auth", 401, `{"error":{"type":"AuthError"}}`, "", "authentication_failed", 401, 0, false},
		{"entitlement", 403, `{"error":{"type":"EntitlementError"}}`, "", "entitlement_required", 403, 0, false},
		{"region", 403, `{"error":{"type":"RegionError"}}`, "", "region_restricted", 403, 0, false},
		{"policy", 403, `{"error":{"type":"DataPolicyError"}}`, "", "data_policy_restricted", 403, 0, false},
		{"legacy credits", 401, `{"error":{"type":"CreditsError"}}`, "", "entitlement_required", 403, 0, false},
		{"legacy monthly", 401, `{"error":{"type":"MonthlyLimitError"}}`, "", "entitlement_required", 403, 0, false},
		{"legacy user limit", 401, `{"error":{"type":"UserLimitError"}}`, "", "entitlement_required", 403, 0, false},
		{"unsupported model", 401, `{"error":{"type":"ModelError"}}`, "", "model_unavailable", 404, 0, false},
		{"cloudflare json", 403, `{"error_code":1010,"error_name":"browser_signature_banned","retryable":false}`, "", "upstream_access_blocked", 502, 5, true},
		{"cloudflare html", 403, `<html>error code: 1010</html>`, "", "upstream_access_blocked", 502, 5, true},
		{"upstream failure", 503, `private-value`, "15", "upstream_error", 503, 15, true},
		{"malformed auth", 401, `private-value`, "", "authentication_failed", 401, 0, false},
		{"generic forbidden", 403, `private-value`, "", "forbidden", 403, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyAt(pluginapi.HTTPResponse{StatusCode: tt.status, Body: []byte(tt.body), Headers: http.Header{"Retry-After": {tt.retry}}}, now)
			var e *pluginabi.Error
			if !errors.As(err, &e) {
				t.Fatalf("not an RPC error: %v", err)
			}
			if e.Code != tt.code || e.HTTPStatus != tt.wantStatus || e.RetryAfterSeconds != tt.seconds || e.Retryable != tt.retryable {
				t.Fatalf("got %+v", e)
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

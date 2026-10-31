package opencodego

import (
	"encoding/json"
	"strings"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func classifyResponse(r pluginapi.HTTPResponse) error { return classifyAt(r, time.Now()) }

// Only allowlisted error types/window names enter public errors. Provider bodies,
// workspace identifiers and messages can contain credentials or personal data.
func classifyAt(r pluginapi.HTTPResponse, now time.Time) error {
	if r.StatusCode >= 200 && r.StatusCode < 300 {
		return nil
	}
	var wire struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
		Metadata struct {
			LimitName string `json:"limitName"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(r.Body, &wire)
	e := &pluginabi.Error{Code: "upstream_error", Message: "OpenCode Go upstream request failed", HTTPStatus: r.StatusCode, Retryable: r.StatusCode == 429 || r.StatusCode >= 500, RetryAfterSeconds: kit.RetryAfterSeconds(r.Headers.Get("Retry-After"), now)}
	if e.HTTPStatus < 400 || e.HTTPStatus > 599 {
		e.HTTPStatus = 502
		e.Retryable = true
	}
	lower := strings.ToLower(string(r.Body))
	if r.StatusCode == 403 && (strings.Contains(lower, "browser_signature_banned") || strings.Contains(lower, `"error_code":1010`) || strings.Contains(lower, `"error_code": 1010`) || strings.Contains(lower, "error code: 1010")) {
		e.Code = "upstream_access_blocked"
		e.Message = "upstream access blocked; no bypass attempted"
		e.HTTPStatus = 502
		e.Retryable = true
	} else {
		switch wire.Error.Type {
		case "GoUsageLimitError":
			e.Code = "quota_exhausted"
			e.Message = "OpenCode Go usage limit reached"
			e.HTTPStatus = 429
			e.Retryable = true
			switch wire.Metadata.LimitName {
			case "5 hour":
				e.Code += "_5h"
			case "weekly":
				e.Code += "_weekly"
			case "monthly":
				e.Code += "_monthly"
			}
			if e.RetryAfterSeconds == 0 {
				e.RetryAfterSeconds = 60
			}
		case "RateLimitError":
			e.Code = "rate_limited"
			e.Message = "OpenCode Go temporarily rate limited"
			e.HTTPStatus = 429
			e.Retryable = true
		case "AuthError":
			e.Code = "authentication_failed"
			e.HTTPStatus = 401
			e.Retryable = false
		case "EntitlementError", "CreditsError", "MonthlyLimitError", "UserLimitError":
			e.Code = "entitlement_required"
			e.HTTPStatus = 403
			e.Retryable = false
		case "RegionError":
			e.Code = "region_restricted"
			e.HTTPStatus = 403
			e.Retryable = false
		case "DataPolicyError":
			e.Code = "data_policy_restricted"
			e.HTTPStatus = 403
			e.Retryable = false
		case "ModelError":
			e.Code = "model_unavailable"
			e.HTTPStatus = 404
			e.Retryable = false
		default:
			switch r.StatusCode {
			case 401:
				e.Code = "authentication_failed"
			case 403:
				e.Code = "forbidden"
			case 429:
				e.Code = "rate_limited"
			}
		}
	}
	if e.Retryable && e.RetryAfterSeconds == 0 {
		e.RetryAfterSeconds = 5
	}
	if !e.Retryable {
		e.RetryAfterSeconds = 0
	}
	return e
}

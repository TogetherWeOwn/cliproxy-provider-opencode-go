package opencodego

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// newQuota uses the original subscription-pool parser; unlike the legacy
// fetcher, the kit owns the bounded, non-redirecting transport.
func newQuota(base string, transport http.RoundTripper) *kit.CachedQuota {
	client := kit.NewQuotaHTTPClient(transport)
	return kit.NewCachedQuota("opencode-go", "OpenCode Go", func(ctx context.Context, r pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		fields, err := kit.AuthFields(r.StorageJSON, r.Metadata)
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		key, ok := fields["quota_api_key"].(string)
		if !ok || strings.TrimSpace(key) == "" {
			key, err = kit.APIKey(r.StorageJSON, r.Metadata)
			if err != nil {
				return pluginapi.QuotaFetchResponse{}, err
			}
		}
		headers := http.Header{"Authorization": []string{"Bearer " + strings.TrimSpace(key)}, "Accept": []string{"application/json"}}
		response, err := client.Do(ctx, pluginapi.HTTPRequest{URL: base + "/inference/go/v1/usage", Headers: headers})
		var pe *pluginabi.Error
		if errors.As(err, &pe) && (pe.HTTPStatus == 404 || pe.HTTPStatus == 410) {
			response, err = client.Do(ctx, pluginapi.HTTPRequest{URL: base + "/zen/go/v1/usage", Headers: headers})
		}
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		snap, err := parseUsageResponse(response.Body, time.Now())
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, &pluginabi.Error{Code: "invalid_quota", Message: "invalid quota response", HTTPStatus: 502}
		}
		buckets := make([]pluginapi.QuotaBucket, 0, 3)
		for _, item := range []struct {
			name   string
			window usageWindow
		}{{"5h", snap.FiveHour}, {"weekly", snap.Weekly}, {"monthly", snap.Monthly}} {
			buckets = append(buckets, pluginapi.QuotaBucket{Window: item.name, RemainingFraction: 1 - item.window.Utilization, ResetTime: item.window.ResetAt.Format(time.RFC3339)})
		}
		return pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{DisplayName: "OpenCode Go", Buckets: buckets}}}, nil
	})
}

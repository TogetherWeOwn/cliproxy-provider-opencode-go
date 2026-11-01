package opencodego

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestQuotaProviderUsesQuotaKeyAndNormalizes(t *testing.T) {
	now := time.Now().UTC()
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/inference/go/v1/usage" || r.Header.Get("Authorization") != "Bearer example-quota-key" {
			t.Errorf("unexpected request")
		}
		fmt.Fprint(w, usageFixture(usageWindowFixture("ok", 42, now.Add(time.Hour)), usageWindowFixture("rate-limited", 0, now.Add(24*time.Hour)), usageWindowFixture("ok", 5, now.Add(30*24*time.Hour))))
	}))
	defer s.Close()
	q := newQuota(s.URL, nil)
	req := pluginapi.QuotaFetchRequest{AuthID: "account", StorageJSON: []byte(`{"api_key":"example-key","quota_api_key":"example-quota-key"}`)}
	got, err := q.FetchQuota(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 1 || len(got.Groups[0].Buckets) != 3 || math.Abs(got.Groups[0].Buckets[0].RemainingFraction-0.58) > 1e-9 || got.Groups[0].Buckets[1].RemainingFraction != 0 {
		t.Fatalf("unexpected quota: %+v", got)
	}
	windows := []string{got.Groups[0].Buckets[0].Window, got.Groups[0].Buckets[1].Window, got.Groups[0].Buckets[2].Window}
	if windows[0] != "5-hour limit" || windows[1] != "Weekly limit" || windows[2] != "Monthly limit" {
		t.Fatalf("unexpected bucket labels: %+v", got.Groups[0].Buckets)
	}
	if v := providerkit.CheckCard(got); len(v) != 0 {
		t.Fatalf("card style violations: %v", v)
	}
	_, err = q.FetchQuota(context.Background(), req)
	if err != nil || calls != 1 {
		t.Fatalf("cache: calls=%d err=%v", calls, err)
	}
}

// Utilization is visible as the bar fill, so per-window usage chips would
// only repeat the bars: the card shows no summary.
func TestQuotaProviderOmitsSummaryMetrics(t *testing.T) {
	now := time.Now().UTC()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, usageFixture(usageWindowFixture("ok", 42, now.Add(time.Hour)), usageWindowFixture("rate-limited", 0, now.Add(24*time.Hour)), usageWindowFixture("ok", 5, now.Add(30*24*time.Hour))))
	}))
	defer s.Close()
	q := newQuota(s.URL, nil)
	got, err := q.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{AuthID: "summary", StorageJSON: []byte(`{"api_key":"example-key"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Summary) != 0 {
		t.Fatalf("summary = %+v, want no metrics", got.Summary)
	}
	if v := providerkit.CheckCard(got); len(v) != 0 {
		t.Fatalf("card style violations: %v", v)
	}
}

// A malformed rolling window drops just that bucket; the healthy
// windows still feed wait/resume.
func TestQuotaProviderDropsOnlyMalformedWindow(t *testing.T) {
	now := time.Now().UTC()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, usageFixture(`{"status":"bogus","percent":1,"resetsAt":"`+now.Add(time.Hour).Format(time.RFC3339)+`"}`, usageWindowFixture("ok", 10, now.Add(7*24*time.Hour)), usageWindowFixture("ok", 5, now.Add(30*24*time.Hour))))
	}))
	defer s.Close()
	q := newQuota(s.URL, nil)
	got, err := q.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{AuthID: "partial", StorageJSON: []byte(`{"api_key":"example-key"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 1 || len(got.Groups[0].Buckets) != 2 {
		t.Fatalf("buckets = %+v, want weekly + monthly only", got.Groups)
	}
	for _, bucket := range got.Groups[0].Buckets {
		if bucket.Window == "5-hour limit" || bucket.ResetTime == "" {
			t.Fatalf("unexpected bucket: %+v", bucket)
		}
	}
	if len(got.Summary) != 0 {
		t.Fatalf("summary = %+v, want no metrics", got.Summary)
	}
}

func TestQuotaFallsBackOnlyWhenEndpointMissing(t *testing.T) {
	for _, status := range []int{404, 401} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == "/inference/go/v1/usage" {
					w.WriteHeader(status)
					return
				}
				now := time.Now().UTC()
				fmt.Fprint(w, usageFixture(usageWindowFixture("ok", 1, now.Add(time.Hour)), usageWindowFixture("ok", 1, now.Add(time.Hour)), usageWindowFixture("ok", 1, now.Add(time.Hour))))
			}))
			defer s.Close()
			_, err := newQuota(s.URL, nil).FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{AuthID: "one", StorageJSON: []byte(`{"api_key":"example"}`)})
			if status == 404 && (err != nil || calls != 2) || status == 401 && (err == nil || calls != 1) {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

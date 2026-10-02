package cloudfrontip

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

const sample = `{
  "syncToken": "1",
  "prefixes": [
    {"ip_prefix": "3.172.0.0/18", "region": "GLOBAL", "service": "CLOUDFRONT_ORIGIN_FACING"},
    {"ip_prefix": "13.32.0.0/15", "region": "GLOBAL", "service": "CLOUDFRONT"},
    {"ip_prefix": "15.158.0.0/16", "region": "GLOBAL", "service": "CLOUDFRONT_ORIGIN_FACING"}
  ],
  "ipv6_prefixes": [
    {"ipv6_prefix": "2600:9000::/28", "region": "GLOBAL", "service": "CLOUDFRONT"},
    {"ipv6_prefix": "2600:9000:f000::/36", "region": "GLOBAL", "service": "CLOUDFRONT_ORIGIN_FACING"}
  ]
}`

func fakeAWS(t *testing.T, fail *atomic.Bool, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func provision(t *testing.T, s *CloudFrontIPRange) error {
	t.Helper()
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	return s.Provision(ctx)
}

func prefixes(exprs ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(exprs))
	for _, e := range exprs {
		out = append(out, netip.MustParsePrefix(e))
	}
	return out
}

func assertRanges(t *testing.T, s *CloudFrontIPRange, want []netip.Prefix) {
	t.Helper()
	got := s.GetIPRanges(nil)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestProvisionKeepsOnlyOriginFacing(t *testing.T) {
	var fail atomic.Bool
	s := &CloudFrontIPRange{url: fakeAWS(t, &fail, sample).URL}
	if err := provision(t, s); err != nil {
		t.Fatal(err)
	}
	assertRanges(t, s, prefixes("3.172.0.0/18", "15.158.0.0/16", "2600:9000:f000::/36"))
}

func TestFailedRefreshKeepsLastRanges(t *testing.T) {
	var fail atomic.Bool
	s := &CloudFrontIPRange{url: fakeAWS(t, &fail, sample).URL}
	if err := provision(t, s); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if err := s.refresh(context.Background()); err == nil {
		t.Fatal("refresh against a failing server succeeded")
	}
	assertRanges(t, s, prefixes("3.172.0.0/18", "15.158.0.0/16", "2600:9000:f000::/36"))
}

func TestFirstFetchFailureUsesFallback(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	s := &CloudFrontIPRange{
		url:      fakeAWS(t, &fail, sample).URL,
		Fallback: []string{"3.29.57.0/26", "13.124.199.1"},
	}
	if err := provision(t, s); err != nil {
		t.Fatal(err)
	}
	assertRanges(t, s, prefixes("3.29.57.0/26", "13.124.199.1/32"))

	fail.Store(false)
	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertRanges(t, s, prefixes("3.172.0.0/18", "15.158.0.0/16", "2600:9000:f000::/36"))
}

func TestFirstFetchFailureWithoutFallbackFails(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	s := &CloudFrontIPRange{url: fakeAWS(t, &fail, sample).URL}
	if err := provision(t, s); err == nil {
		t.Fatal("provision succeeded without ranges")
	}
}

func TestEmptyListIsAnError(t *testing.T) {
	var fail atomic.Bool
	s := &CloudFrontIPRange{url: fakeAWS(t, &fail, `{"prefixes": [], "ipv6_prefixes": []}`).URL}
	if err := provision(t, s); err == nil {
		t.Fatal("provision succeeded with an empty list")
	}
}

func TestBadFallbackFails(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	s := &CloudFrontIPRange{
		url:      fakeAWS(t, &fail, sample).URL,
		Fallback: []string{"not-an-ip"},
	}
	if err := provision(t, s); err == nil {
		t.Fatal("provision succeeded with a bad fallback")
	}
}

func TestUnmarshalCaddyfile(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    *CloudFrontIPRange
		wantErr bool
	}{
		{name: "bare", input: `cloudfront`, want: &CloudFrontIPRange{}},
		{
			name: "everything",
			input: `cloudfront {
				interval 1h
				timeout 5s
				fallback 3.29.57.0/26 3.172.0.0/18
				fallback 15.158.0.0/16
			}`,
			want: &CloudFrontIPRange{
				Interval: caddy.Duration(3600e9),
				Timeout:  caddy.Duration(5e9),
				Fallback: []string{"3.29.57.0/26", "3.172.0.0/18", "15.158.0.0/16"},
			},
		},
		{name: "argument", input: `cloudfront 1h`, wantErr: true},
		{name: "interval without value", input: "cloudfront {\ninterval\n}", wantErr: true},
		{name: "zero interval", input: "cloudfront {\ninterval 0s\n}", wantErr: true},
		{name: "two timeouts", input: "cloudfront {\ntimeout 1s 2s\n}", wantErr: true},
		{name: "unknown", input: "cloudfront {\nurl x\n}", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := &CloudFrontIPRange{}
			err := got.UnmarshalCaddyfile(caddyfile.NewTestDispenser(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("no error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Interval != tt.want.Interval || got.Timeout != tt.want.Timeout ||
				len(got.Fallback) != len(tt.want.Fallback) {
				t.Fatalf("got %v %v %v, want %v %v %v", got.Interval, got.Timeout, got.Fallback,
					tt.want.Interval, tt.want.Timeout, tt.want.Fallback)
			}
			for i := range tt.want.Fallback {
				if got.Fallback[i] != tt.want.Fallback[i] {
					t.Fatalf("got %v, want %v", got.Fallback, tt.want.Fallback)
				}
			}
		})
	}
}

// Package cloudfrontip provides a Caddy IP source of CloudFront's
// origin-facing ranges, kept current from AWS's published list.
package cloudfrontip

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

const (
	rangesURL = "https://ip-ranges.amazonaws.com/ip-ranges.json"
	service   = "CLOUDFRONT_ORIGIN_FACING"
	// The list rarely changes, so twice a day is plenty.
	defaultInterval = caddy.Duration(12 * time.Hour)
	defaultTimeout  = caddy.Duration(30 * time.Second)
)

func init() {
	caddy.RegisterModule(new(CloudFrontIPRange))
}

// CloudFrontIPRange gives the CLOUDFRONT_ORIGIN_FACING ranges from
// ip-ranges.amazonaws.com, fetched at provision and every Interval.
// A failed refresh keeps the last list.
type CloudFrontIPRange struct {
	// How often to fetch the list. Default: 12h.
	Interval caddy.Duration `json:"interval,omitempty"`
	// How long one fetch may take. Default: 30s.
	Timeout caddy.Duration `json:"timeout,omitempty"`
	// Ranges to use when the first fetch fails. Without them,
	// a failed first fetch fails provisioning.
	Fallback []string `json:"fallback,omitempty"`

	url    string
	client *http.Client
	logger *zap.Logger
	ranges atomic.Pointer[[]netip.Prefix]
}

// CaddyModule returns the Caddy module information.
func (*CloudFrontIPRange) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.ip_sources.cloudfront",
		New: func() caddy.Module { return new(CloudFrontIPRange) },
	}
}

// Provision fetches the list and starts refreshing it.
func (s *CloudFrontIPRange) Provision(ctx caddy.Context) error {
	s.logger = ctx.Logger()
	if s.url == "" {
		s.url = rangesURL
	}
	if s.Interval == 0 {
		s.Interval = defaultInterval
	}
	if s.Timeout == 0 {
		s.Timeout = defaultTimeout
	}
	if s.client == nil {
		s.client = &http.Client{Timeout: time.Duration(s.Timeout)}
	}

	if err := s.refresh(ctx); err != nil {
		if len(s.Fallback) == 0 {
			return err
		}
		s.logger.Warn("using fallback ranges", zap.Error(err))
		fallback, err := parsePrefixes(s.Fallback)
		if err != nil {
			return err
		}
		s.setRanges(fallback)
	}

	go s.refreshLoop(ctx)
	return nil
}

func (s *CloudFrontIPRange) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(s.Interval))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.refresh(ctx); err != nil {
				s.logger.Warn("keeping the last ranges", zap.Error(err))
			}
		}
	}
}

func (s *CloudFrontIPRange) refresh(ctx context.Context) error {
	ranges, err := s.fetch(ctx)
	if err != nil {
		return err
	}
	s.setRanges(ranges)
	s.logger.Info("fetched ranges", zap.Int("count", len(ranges)))
	return nil
}

type ipRanges struct {
	Prefixes []struct {
		IPPrefix string `json:"ip_prefix"`
		Service  string `json:"service"`
	} `json:"prefixes"`
	IPv6Prefixes []struct {
		IPv6Prefix string `json:"ipv6_prefix"`
		Service    string `json:"service"`
	} `json:"ipv6_prefixes"`
}

func (s *CloudFrontIPRange) fetch(ctx context.Context) ([]netip.Prefix, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", s.url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: %s", s.url, resp.Status)
	}

	var body ipRanges
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", s.url, err)
	}
	var exprs []string
	for _, p := range body.Prefixes {
		if p.Service == service {
			exprs = append(exprs, p.IPPrefix)
		}
	}
	for _, p := range body.IPv6Prefixes {
		if p.Service == service {
			exprs = append(exprs, p.IPv6Prefix)
		}
	}
	// Trusting nothing would make every client look like a CloudFront edge.
	if len(exprs) == 0 {
		return nil, fmt.Errorf("no %s ranges in %s", service, s.url)
	}
	return parsePrefixes(exprs)
}

func parsePrefixes(exprs []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(exprs))
	for _, expr := range exprs {
		prefix, err := caddyhttp.CIDRExpressionToPrefix(expr)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

func (s *CloudFrontIPRange) setRanges(ranges []netip.Prefix) {
	s.ranges.Store(&ranges)
}

// GetIPRanges returns the current ranges.
func (s *CloudFrontIPRange) GetIPRanges(_ *http.Request) []netip.Prefix {
	return *s.ranges.Load()
}

// UnmarshalCaddyfile sets up the module from Caddyfile tokens:
//
//	cloudfront {
//		interval <duration>
//		timeout <duration>
//		fallback <ranges...>
//	}
func (s *CloudFrontIPRange) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		switch d.Val() {
		case "interval":
			if err := unmarshalDuration(d, &s.Interval); err != nil {
				return err
			}
		case "timeout":
			if err := unmarshalDuration(d, &s.Timeout); err != nil {
				return err
			}
		case "fallback":
			s.Fallback = append(s.Fallback, d.RemainingArgs()...)
		default:
			return d.Errf("unknown subdirective %q", d.Val())
		}
	}
	return nil
}

func unmarshalDuration(d *caddyfile.Dispenser, dst *caddy.Duration) error {
	name := d.Val()
	if !d.NextArg() {
		return d.ArgErr()
	}
	dur, err := caddy.ParseDuration(d.Val())
	if err != nil {
		return d.Errf("parsing %s: %v", name, err)
	}
	if dur <= 0 {
		return d.Errf("%s must be positive", name)
	}
	*dst = caddy.Duration(dur)
	if d.NextArg() {
		return d.ArgErr()
	}
	return nil
}

var (
	_ caddy.Provisioner       = (*CloudFrontIPRange)(nil)
	_ caddyfile.Unmarshaler   = (*CloudFrontIPRange)(nil)
	_ caddyhttp.IPRangeSource = (*CloudFrontIPRange)(nil)
)

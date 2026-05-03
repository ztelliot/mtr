package abuse

import "testing"

func TestLimiterAppliesGlobalLimit(t *testing.T) {
	limiter := NewConfiguredLimiter(RateLimitConfig{
		Global: Limit{RequestsPerMinute: 10, Burst: 1},
		IP:     Limit{RequestsPerMinute: 100, Burst: 100},
	})
	if !limiter.AllowRequest("203.0.113.10") {
		t.Fatal("first request should pass")
	}
	if limiter.AllowRequest("203.0.113.11") {
		t.Fatal("second request should hit global limit")
	}
}

func TestLimiterAppliesExactIPLimit(t *testing.T) {
	limiter := NewConfiguredLimiter(RateLimitConfig{
		Global: Limit{RequestsPerMinute: 100, Burst: 100},
		IP:     Limit{RequestsPerMinute: 10, Burst: 1},
		CIDR:   CIDRLimit{Limit: Limit{RequestsPerMinute: 100, Burst: 100}, IPv4Prefix: 24, IPv6Prefix: 64},
	})
	if !limiter.AllowRequest("203.0.113.10") {
		t.Fatal("first request should pass")
	}
	if limiter.AllowRequest("203.0.113.10") {
		t.Fatal("second request from same IP should hit IP limit")
	}
	if !limiter.AllowRequest("203.0.113.11") {
		t.Fatal("different IP in same CIDR should use its own exact IP bucket")
	}
}

func TestLimiterAppliesCIDRLimit(t *testing.T) {
	limiter := NewConfiguredLimiter(RateLimitConfig{
		Global: Limit{RequestsPerMinute: 100, Burst: 100},
		IP:     Limit{RequestsPerMinute: 100, Burst: 100},
		CIDR:   CIDRLimit{Limit: Limit{RequestsPerMinute: 10, Burst: 1}, IPv4Prefix: 24, IPv6Prefix: 64},
	})
	if !limiter.AllowRequest("203.0.113.10") {
		t.Fatal("first request should pass")
	}
	if limiter.AllowRequest("203.0.113.11") {
		t.Fatal("second request in same CIDR should hit CIDR limit")
	}
	if !limiter.AllowRequest("198.51.100.10") {
		t.Fatal("request outside CIDR should use a different CIDR bucket")
	}
}

func TestLimiterAppliesToolLimit(t *testing.T) {
	limiter := NewConfiguredLimiter(RateLimitConfig{
		Global: Limit{RequestsPerMinute: 100, Burst: 100},
		IP:     Limit{RequestsPerMinute: 100, Burst: 100},
		Tools: map[string]ToolLimit{
			"mtr": {Global: Limit{RequestsPerMinute: 10, Burst: 1}},
		},
	})
	if !limiter.AllowTool("mtr") {
		t.Fatal("first tool request should pass")
	}
	if limiter.AllowTool("mtr") {
		t.Fatal("second tool request should hit tool limit")
	}
	if !limiter.AllowTool("ping") {
		t.Fatal("unconfigured tool should pass")
	}
}

func TestLimiterAppliesGeoIPGlobalOnly(t *testing.T) {
	limiter := NewConfiguredLimiter(RateLimitConfig{
		Global: Limit{RequestsPerMinute: 1, Burst: 1},
		IP:     Limit{RequestsPerMinute: 1, Burst: 1},
		CIDR:   CIDRLimit{Limit: Limit{RequestsPerMinute: 1, Burst: 1}, IPv4Prefix: 24, IPv6Prefix: 64},
		GeoIP:  Limit{RequestsPerMinute: 10, Burst: 2},
	})
	if !limiter.AllowGeoIP() {
		t.Fatal("first geoip request should pass")
	}
	if !limiter.AllowGeoIP() {
		t.Fatal("second geoip request should use its own global bucket")
	}
	if limiter.AllowGeoIP() {
		t.Fatal("third geoip request should hit geoip global limit")
	}
}

func TestGeoIPLimitDoesNotConsumeRequestLimit(t *testing.T) {
	limiter := NewConfiguredLimiter(RateLimitConfig{
		Global: Limit{RequestsPerMinute: 10, Burst: 1},
		IP:     Limit{RequestsPerMinute: 10, Burst: 1},
		CIDR:   CIDRLimit{Limit: Limit{RequestsPerMinute: 10, Burst: 1}, IPv4Prefix: 24, IPv6Prefix: 64},
		GeoIP:  Limit{RequestsPerMinute: 10, Burst: 10},
	})
	if !limiter.AllowGeoIP() || !limiter.AllowGeoIP() {
		t.Fatal("geoip requests should pass independently")
	}
	if !limiter.AllowRequest("203.0.113.10") {
		t.Fatal("first regular request should not be consumed by geoip")
	}
	if limiter.AllowRequest("203.0.113.11") {
		t.Fatal("second regular request should hit regular global limit")
	}
}

func TestLimiterAppliesToolIPLimit(t *testing.T) {
	limiter := NewConfiguredLimiter(RateLimitConfig{
		Global: Limit{RequestsPerMinute: 100, Burst: 100},
		IP:     Limit{RequestsPerMinute: 100, Burst: 100},
		CIDR:   CIDRLimit{Limit: Limit{RequestsPerMinute: 100, Burst: 100}, IPv4Prefix: 24, IPv6Prefix: 64},
		Tools: map[string]ToolLimit{
			"mtr": {IP: Limit{RequestsPerMinute: 10, Burst: 1}},
		},
	})
	if !limiter.AllowTool("mtr", "203.0.113.10") {
		t.Fatal("first tool request should pass")
	}
	if limiter.AllowTool("mtr", "203.0.113.10") {
		t.Fatal("second tool request from same IP should hit tool IP limit")
	}
	if !limiter.AllowTool("mtr", "203.0.113.11") {
		t.Fatal("different IP in same CIDR should use its own tool IP bucket")
	}
}

func TestLimiterAppliesToolCIDRLimit(t *testing.T) {
	limiter := NewConfiguredLimiter(RateLimitConfig{
		Global: Limit{RequestsPerMinute: 100, Burst: 100},
		IP:     Limit{RequestsPerMinute: 100, Burst: 100},
		CIDR:   CIDRLimit{Limit: Limit{RequestsPerMinute: 100, Burst: 100}, IPv4Prefix: 24, IPv6Prefix: 64},
		Tools: map[string]ToolLimit{
			"mtr": {CIDR: Limit{RequestsPerMinute: 10, Burst: 1}},
		},
	})
	if !limiter.AllowTool("mtr", "203.0.113.10") {
		t.Fatal("first tool request should pass")
	}
	if limiter.AllowTool("mtr", "203.0.113.11") {
		t.Fatal("second tool request in same CIDR should hit tool CIDR limit")
	}
}

func TestLimiterAppliesIPv6PrefixLimit(t *testing.T) {
	limiter := NewConfiguredLimiter(RateLimitConfig{
		Global: Limit{RequestsPerMinute: 100, Burst: 100},
		IP:     Limit{RequestsPerMinute: 100, Burst: 100},
		CIDR:   CIDRLimit{Limit: Limit{RequestsPerMinute: 10, Burst: 1}, IPv4Prefix: 32, IPv6Prefix: 64},
	})
	if !limiter.AllowRequest("2001:db8::1") {
		t.Fatal("first IPv6 request should pass")
	}
	if limiter.AllowRequest("2001:db8::2") {
		t.Fatal("second IPv6 request in same /64 should hit limit")
	}
	if !limiter.AllowRequest("2001:db8:1::1") {
		t.Fatal("request outside IPv6 /64 should use a different bucket")
	}
}

func TestLimiterExemptsCIDRsFromRequestLimits(t *testing.T) {
	limiter := NewConfiguredLimiter(RateLimitConfig{
		Global:      Limit{RequestsPerMinute: 10, Burst: 1},
		IP:          Limit{RequestsPerMinute: 10, Burst: 1},
		CIDR:        CIDRLimit{Limit: Limit{RequestsPerMinute: 10, Burst: 1}, IPv4Prefix: 24, IPv6Prefix: 64},
		ExemptCIDRs: []string{"203.0.113.0/24", "2001:db8::/64"},
	})
	if !limiter.AllowRequest("198.51.100.10") {
		t.Fatal("first non-exempt request should pass")
	}
	if limiter.AllowRequest("198.51.100.11") {
		t.Fatal("second non-exempt request should hit global limit")
	}
	for i := 0; i < 3; i++ {
		if !limiter.AllowRequest("203.0.113.10") {
			t.Fatal("exempt IPv4 request should bypass request limits")
		}
		if !limiter.AllowRequest("2001:db8::1") {
			t.Fatal("exempt IPv6 request should bypass request limits")
		}
	}
}

func TestLimiterExemptsCIDRsFromToolLimits(t *testing.T) {
	limiter := NewConfiguredLimiter(RateLimitConfig{
		Global:      Limit{RequestsPerMinute: 100, Burst: 100},
		IP:          Limit{RequestsPerMinute: 100, Burst: 100},
		CIDR:        CIDRLimit{Limit: Limit{RequestsPerMinute: 100, Burst: 100}, IPv4Prefix: 24, IPv6Prefix: 64},
		ExemptCIDRs: []string{"203.0.113.0/24"},
		Tools: map[string]ToolLimit{
			"mtr": {Global: Limit{RequestsPerMinute: 10, Burst: 1}, IP: Limit{RequestsPerMinute: 10, Burst: 1}},
		},
	})
	if !limiter.AllowTool("mtr", "198.51.100.10") {
		t.Fatal("first non-exempt tool request should pass")
	}
	if limiter.AllowTool("mtr", "198.51.100.11") {
		t.Fatal("second non-exempt tool request should hit global tool limit")
	}
	for i := 0; i < 3; i++ {
		if !limiter.AllowTool("mtr", "203.0.113.10") {
			t.Fatal("exempt tool request should bypass tool limits")
		}
	}
}

func TestLimiterRejectsInvalidExemptCIDR(t *testing.T) {
	if _, err := NewConfiguredLimiterWithError(RateLimitConfig{ExemptCIDRs: []string{"not-a-cidr"}}); err == nil {
		t.Fatal("expected invalid exempt CIDR to be rejected")
	}
}

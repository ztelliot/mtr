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

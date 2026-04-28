package abuse

import (
	"net"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type Limit struct {
	RequestsPerMinute int
	Burst             int
}

type RateLimitConfig struct {
	Global Limit
	IP     Limit
	CIDR   CIDRLimit
	Tools  map[string]ToolLimit
}

type CIDRLimit struct {
	Limit
	IPv4Prefix int
	IPv6Prefix int
}

type ToolLimit struct {
	Global Limit
	CIDR   Limit
	IP     Limit
}

type Limiter struct {
	mu         sync.Mutex
	global     Limit
	ip         Limit
	cidrLimit  Limit
	tools      map[string]ToolLimit
	cidr       CIDRLimit
	items      map[string]*entry
	maxAge     time.Duration
	cleanupDue time.Time
}

type entry struct {
	limiter *rate.Limiter
	seen    time.Time
}

func NewConfiguredLimiter(cfg RateLimitConfig) *Limiter {
	cfg.Global = normalizeLimit(cfg.Global, 600, 200)
	cfg.IP = normalizeLimit(cfg.IP, 60, 20)
	cfg.CIDR = normalizeCIDR(cfg.CIDR)
	cfg.CIDR.Limit = normalizeLimit(cfg.CIDR.Limit, 300, 100)
	tools := make(map[string]ToolLimit, len(cfg.Tools))
	for tool, limit := range cfg.Tools {
		tools[tool] = ToolLimit{
			Global: normalizeLimit(limit.Global, 0, 0),
			CIDR:   normalizeLimit(limit.CIDR, 0, 0),
			IP:     normalizeLimit(limit.IP, 0, 0),
		}
	}
	return &Limiter{
		global:    cfg.Global,
		ip:        cfg.IP,
		cidrLimit: cfg.CIDR.Limit,
		tools:     tools,
		cidr:      cfg.CIDR,
		items:     map[string]*entry{},
		maxAge:    10 * time.Minute,
	}
}

func (l *Limiter) AllowRequest(clientIP string) bool {
	if l == nil {
		return true
	}
	if !l.allow("global", l.global) {
		return false
	}
	if !l.allow("cidr:"+l.cidrKey(clientIP), l.cidrLimit) {
		return false
	}
	return l.allow("ip:"+l.ipKey(clientIP), l.ip)
}

func (l *Limiter) AllowTool(tool string, clientIP ...string) bool {
	if l == nil {
		return true
	}
	limit, ok := l.tools[tool]
	if !ok {
		return true
	}
	if !l.allow("tool:global:"+tool, limit.Global) {
		return false
	}
	if len(clientIP) == 0 {
		return true
	}
	if !l.allow("tool:cidr:"+tool+":"+l.cidrKey(clientIP[0]), limit.CIDR) {
		return false
	}
	return l.allow("tool:ip:"+tool+":"+l.ipKey(clientIP[0]), limit.IP)
}

func (l *Limiter) allow(key string, limit Limit) bool {
	if limit.RequestsPerMinute <= 0 || limit.Burst <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.After(l.cleanupDue) {
		for k, e := range l.items {
			if now.Sub(e.seen) > l.maxAge {
				delete(l.items, k)
			}
		}
		l.cleanupDue = now.Add(time.Minute)
	}
	e := l.items[key]
	if e == nil {
		e = &entry{limiter: rate.NewLimiter(rate.Every(time.Minute/time.Duration(limit.RequestsPerMinute)), limit.Burst)}
		l.items[key] = e
	}
	e.seen = now
	return e.limiter.Allow()
}

func (l *Limiter) ipKey(raw string) string {
	ip := net.ParseIP(raw)
	if ip == nil {
		return raw
	}
	return ip.String()
}

func (l *Limiter) cidrKey(raw string) string {
	ip := net.ParseIP(raw)
	if ip == nil {
		return raw
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		network := net.IPNet{IP: ipv4.Mask(net.CIDRMask(l.cidr.IPv4Prefix, 32)), Mask: net.CIDRMask(l.cidr.IPv4Prefix, 32)}
		return network.String()
	}
	if ipv6 := ip.To16(); ipv6 != nil {
		network := net.IPNet{IP: ipv6.Mask(net.CIDRMask(l.cidr.IPv6Prefix, 128)), Mask: net.CIDRMask(l.cidr.IPv6Prefix, 128)}
		return network.String()
	}
	return ip.String()
}

func normalizeCIDR(cidr CIDRLimit) CIDRLimit {
	if cidr.IPv4Prefix == 0 {
		cidr.IPv4Prefix = 32
	}
	if cidr.IPv6Prefix == 0 {
		cidr.IPv6Prefix = 128
	}
	if cidr.IPv4Prefix < 0 {
		cidr.IPv4Prefix = 32
	}
	if cidr.IPv4Prefix > 32 {
		cidr.IPv4Prefix = 32
	}
	if cidr.IPv6Prefix < 0 {
		cidr.IPv6Prefix = 128
	}
	if cidr.IPv6Prefix > 128 {
		cidr.IPv6Prefix = 128
	}
	return cidr
}

func normalizeLimit(limit Limit, defaultRPM, defaultBurst int) Limit {
	if limit.RequestsPerMinute == 0 {
		limit.RequestsPerMinute = defaultRPM
	}
	if limit.Burst == 0 {
		limit.Burst = defaultBurst
	}
	if limit.RequestsPerMinute > 0 && limit.Burst == 0 {
		limit.Burst = 1
	}
	return limit
}

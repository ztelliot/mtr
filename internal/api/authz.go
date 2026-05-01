package api

import (
	"context"
	"fmt"
	"regexp"
	"sort"

	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
)

type TokenConfig struct {
	Token string
	Scope TokenScope
}

type TokenScope struct {
	All            bool
	ScheduleAccess ScheduleAccess
	Agents         []string
	Tools          map[model.Tool]ToolScope
}

type ScheduleAccess string

const (
	ScheduleAccessNone  ScheduleAccess = "none"
	ScheduleAccessRead  ScheduleAccess = "read"
	ScheduleAccessWrite ScheduleAccess = "write"
)

type ToolScope struct {
	AllowedArgs    map[string]string
	ResolveOnAgent *bool
	IPVersions     []model.IPVersion
}

type PermissionsResponse struct {
	Tools          map[model.Tool]ToolPermission `json:"tools"`
	Agents         []string                      `json:"agents"`
	ScheduleAccess ScheduleAccess                `json:"schedule_access"`
}

type ToolPermission struct {
	AllowedArgs    map[string]string `json:"allowed_args,omitempty"`
	ResolveOnAgent *bool             `json:"resolve_on_agent,omitempty"`
	IPVersions     []model.IPVersion `json:"ip_versions,omitempty"`
	RequiresAgent  bool              `json:"requires_agent"`
}

type principal struct {
	token string
	scope TokenScope
}

type principalContextKey struct{}

var allTokenScope = TokenScope{All: true, ScheduleAccess: ScheduleAccessWrite, Agents: []string{"*"}}

func normalizeTokenScope(scope TokenScope) TokenScope {
	if scope.All {
		scope.ScheduleAccess = ScheduleAccessWrite
		if len(scope.Agents) == 0 {
			scope.Agents = []string{"*"}
		}
		return scope
	}
	if scope.ScheduleAccess == "" {
		scope.ScheduleAccess = ScheduleAccessNone
	}
	if scope.Tools == nil {
		scope.Tools = map[model.Tool]ToolScope{}
	}
	return scope
}

func withPrincipal(ctx context.Context, p principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

func principalFromContext(ctx context.Context) principal {
	if p, ok := ctx.Value(principalContextKey{}).(principal); ok {
		return p
	}
	return principal{scope: allTokenScope}
}

func (s *Server) authorizeJob(ctx context.Context, req model.CreateJobRequest) error {
	scope := principalFromContext(ctx).scope
	if scope.All {
		return nil
	}
	toolScope, ok := scope.Tools[req.Tool]
	if !ok {
		return fmt.Errorf("tool %q is not allowed", req.Tool)
	}
	if len(toolScope.IPVersions) > 0 && !containsIPVersion(toolScope.IPVersions, req.IPVersion) {
		return fmt.Errorf("ip_version %d is not allowed for %s", req.IPVersion, req.Tool)
	}
	if toolScope.ResolveOnAgent != nil && req.ResolveOnAgent != *toolScope.ResolveOnAgent {
		return fmt.Errorf("resolve_on_agent=%t is not allowed for %s", req.ResolveOnAgent, req.Tool)
	}
	if req.AgentID != "" && !scopeAllowsAgent(scope, req.AgentID) {
		return fmt.Errorf("agent %q is not allowed", req.AgentID)
	}
	if toolScope.AllowedArgs != nil {
		for key, value := range effectiveArgs(req) {
			pattern, ok := toolScope.AllowedArgs[key]
			if !ok {
				return fmt.Errorf("argument %q is not allowed for %s", key, req.Tool)
			}
			if pattern != "" {
				re, err := regexp.Compile(pattern)
				if err != nil {
					return fmt.Errorf("argument %q has invalid permission pattern", key)
				}
				if !re.MatchString(value) {
					return fmt.Errorf("argument %q is invalid", key)
				}
			}
		}
	}
	return nil
}

func (s *Server) permissionsForContext(ctx context.Context) PermissionsResponse {
	scope := principalFromContext(ctx).scope
	tools := map[model.Tool]ToolPermission{}
	for _, tool := range allTools() {
		p, ok := s.policies.Get(tool)
		if !ok {
			continue
		}
		toolScope, allowed := scope.Tools[tool]
		if !scope.All && !allowed {
			continue
		}
		tools[tool] = ToolPermission{
			AllowedArgs:    permissionArgs(p, toolScope, scope.All),
			ResolveOnAgent: toolScope.ResolveOnAgent,
			IPVersions:     permissionIPVersions(toolScope, scope.All),
			RequiresAgent:  requiresPinnedAgent(tool),
		}
	}
	return PermissionsResponse{
		Tools:          tools,
		Agents:         permissionAgents(scope),
		ScheduleAccess: effectiveScheduleAccess(scope),
	}
}

func (s *Server) scheduleReadAllowed(ctx context.Context) bool {
	scope := principalFromContext(ctx).scope
	access := effectiveScheduleAccess(scope)
	return access == ScheduleAccessRead || access == ScheduleAccessWrite
}

func (s *Server) scheduleWriteAllowed(ctx context.Context) bool {
	scope := principalFromContext(ctx).scope
	return effectiveScheduleAccess(scope) == ScheduleAccessWrite
}

func effectiveScheduleAccess(scope TokenScope) ScheduleAccess {
	if scope.All {
		return ScheduleAccessWrite
	}
	switch scope.ScheduleAccess {
	case ScheduleAccessRead, ScheduleAccessWrite:
		return scope.ScheduleAccess
	default:
		return ScheduleAccessNone
	}
}

func scopeAllowsAgent(scope TokenScope, agentID string) bool {
	if scopeAllowsAllAgents(scope) {
		return true
	}
	for _, allowed := range scope.Agents {
		if allowed == agentID {
			return true
		}
	}
	return false
}

func scopeAllowsAllAgents(scope TokenScope) bool {
	if scope.All || len(scope.Agents) == 0 {
		return true
	}
	for _, allowed := range scope.Agents {
		if allowed == "*" {
			return true
		}
	}
	return false
}

func scopeRestrictsAgents(scope TokenScope) bool {
	return !scopeAllowsAllAgents(scope)
}

func permissionAgents(scope TokenScope) []string {
	if scopeAllowsAllAgents(scope) {
		return []string{"*"}
	}
	out := append([]string(nil), scope.Agents...)
	sort.Strings(out)
	return out
}

func permissionArgs(p policy.Policy, toolScope ToolScope, all bool) map[string]string {
	if all || toolScope.AllowedArgs == nil {
		return cloneStringMap(p.AllowedArgs)
	}
	out := map[string]string{}
	for key, pattern := range toolScope.AllowedArgs {
		if _, ok := p.AllowedArgs[key]; ok {
			out[key] = pattern
		}
	}
	return out
}

func permissionIPVersions(toolScope ToolScope, all bool) []model.IPVersion {
	if all || len(toolScope.IPVersions) == 0 {
		return []model.IPVersion{model.IPAny, model.IPv4, model.IPv6}
	}
	out := append([]model.IPVersion(nil), toolScope.IPVersions...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func effectiveArgs(req model.CreateJobRequest) map[string]string {
	out := map[string]string{}
	for key, value := range req.Args {
		if isServerOwnedArg(key) {
			continue
		}
		out[key] = value
	}
	switch req.Tool {
	case model.ToolPing, model.ToolTraceroute, model.ToolMTR:
		if out["protocol"] == "" {
			out["protocol"] = "icmp"
		}
	case model.ToolHTTP:
		if out["method"] == "" {
			out["method"] = "GET"
		}
	case model.ToolDNS:
		if out["type"] == "" {
			out["type"] = "A"
		}
	}
	return out
}

func isServerOwnedArg(key string) bool {
	return key == "count" || key == "max_hops" || key == "max_hop"
}

func containsIPVersion(versions []model.IPVersion, version model.IPVersion) bool {
	for _, item := range versions {
		if item == version {
			return true
		}
	}
	return false
}

func allTools() []model.Tool {
	return []model.Tool{model.ToolPing, model.ToolTraceroute, model.ToolMTR, model.ToolHTTP, model.ToolDNS, model.ToolPort}
}

func requiresPinnedAgent(tool model.Tool) bool {
	return tool == model.ToolTraceroute || tool == model.ToolMTR
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

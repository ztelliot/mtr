package api

import (
	"context"
	"errors"
	"fmt"
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
	ManageAccess   ScheduleAccess
	Agents         []string
	DeniedAgents   []string
	AgentTags      []string
	DeniedTags     []string
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
	ScheduleAccess ScheduleAccess                `json:"schedule_access"`
	ManageAccess   ScheduleAccess                `json:"manage_access"`
}

type ToolPermission = model.AgentToolPermission

type principal struct {
	token string
	scope TokenScope
}

type principalContextKey struct{}

var allTokenScope = TokenScope{All: true, ScheduleAccess: ScheduleAccessWrite, ManageAccess: ScheduleAccessWrite, Agents: []string{"*"}}

func normalizeTokenScope(scope TokenScope) TokenScope {
	if scope.All {
		scope.ScheduleAccess = ScheduleAccessWrite
		scope.ManageAccess = ScheduleAccessWrite
		if len(scope.Agents) == 0 {
			scope.Agents = []string{"*"}
		}
		return scope
	}
	if scope.ScheduleAccess == "" {
		scope.ScheduleAccess = ScheduleAccessNone
	}
	if scope.ManageAccess == "" {
		scope.ManageAccess = ScheduleAccessNone
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
	policies := s.policiesForJob(ctx, req)
	toolPolicy, ok := policies.Get(req.Tool)
	if !ok {
		return fmt.Errorf("tool %q is not allowed", req.Tool)
	}
	if len(toolScope.IPVersions) > 0 && !containsIPVersion(toolScope.IPVersions, req.IPVersion) {
		return fmt.Errorf("ip_version %d is not allowed for %s", req.IPVersion, req.Tool)
	}
	if toolScope.ResolveOnAgent != nil && req.ResolveOnAgent != *toolScope.ResolveOnAgent {
		return fmt.Errorf("resolve_on_agent=%t is not allowed for %s", req.ResolveOnAgent, req.Tool)
	}
	if req.AgentID != "" {
		agents, err := s.agentsWithManagedLabels(ctx)
		if err != nil {
			return err
		}
		allowed := false
		for _, agent := range agents {
			if agent.ID == req.AgentID {
				allowed = scopeAllowsAgentRecord(scope, agent)
				break
			}
		}
		if !allowed {
			return fmt.Errorf("agent %q is not allowed", req.AgentID)
		}
	}
	if toolScope.AllowedArgs != nil {
		allowedArgs := policy.IntersectAllowedArgs(toolPolicy.AllowedArgs, toolScope.AllowedArgs)
		for key, value := range effectiveArgs(req) {
			pattern, ok := allowedArgs[key]
			if !ok {
				return fmt.Errorf("argument %q is not allowed for %s", key, req.Tool)
			}
			if key == "port" {
				if err := policy.ValidatePortAllowed(pattern, value); err != nil {
					if errors.Is(err, policy.ErrInvalidPortRangeRule) {
						return fmt.Errorf("argument %q has invalid permission pattern", key)
					}
					return fmt.Errorf("argument %q is invalid", key)
				}
				continue
			}
			if pattern != "" && !policy.AllowedArgContains(pattern, value) {
				return fmt.Errorf("argument %q is invalid", key)
			}
		}
	}
	return nil
}

func (s *Server) permissionsForContext(ctx context.Context) PermissionsResponse {
	scope := principalFromContext(ctx).scope
	tools := permissionsForPolicies(s.policiesForLabels([]string{model.AgentAllLabel}), scope)
	return PermissionsResponse{
		Tools:          tools,
		ScheduleAccess: effectiveScheduleAccess(scope),
		ManageAccess:   effectiveManageAccess(scope),
	}
}

func (s *Server) agentToolPermissions(agent model.Agent, scope TokenScope) map[model.Tool]ToolPermission {
	agentScope := scope
	if !scope.All {
		agentScope.Tools = allowedAgentToolsScope(s.policiesForLabels([]string{model.AgentAllLabel}), scope)
	}
	tools := permissionsForPolicies(s.policiesForLabels(agent.Labels), agentScope)
	for tool, permission := range tools {
		if !agentSupportsToolCapability(agent, tool) {
			delete(tools, tool)
			continue
		}
		if len(permission.IPVersions) > 0 {
			filtered := permissionIPVersionsForAgent(permission.IPVersions, agent)
			if len(filtered) == 0 {
				delete(tools, tool)
				continue
			}
			permission.IPVersions = filtered
			tools[tool] = permission
		}
	}
	return tools
}

func allowedAgentToolsScope(policies policy.Set, scope TokenScope) map[model.Tool]ToolScope {
	out := make(map[model.Tool]ToolScope, len(scope.Tools))
	for _, tool := range allTools() {
		toolScope, ok := scope.Tools[tool]
		if !ok {
			continue
		}
		if _, ok := policies.Get(tool); !ok {
			continue
		}
		if toolScope.AllowedArgs != nil {
			toolScope.AllowedArgs = cloneStringMap(toolScope.AllowedArgs)
		}
		out[tool] = toolScope
	}
	return out
}

func permissionsForPolicies(policies policy.Set, scope TokenScope) map[model.Tool]ToolPermission {
	tools := map[model.Tool]ToolPermission{}
	for _, tool := range allTools() {
		p, ok := policies.Get(tool)
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
	return tools
}

func agentSupportsToolCapability(agent model.Agent, tool model.Tool) bool {
	for _, capability := range agent.Capabilities {
		if capability == tool {
			return true
		}
	}
	return false
}

func permissionIPVersionsForAgent(versions []model.IPVersion, agent model.Agent) []model.IPVersion {
	protocols := agent.Protocols
	if protocols == 0 {
		protocols = model.ProtocolAll
	}
	out := make([]model.IPVersion, 0, len(versions))
	for _, version := range versions {
		required := policy.RequiredProtocol(version)
		if required == 0 || protocols&required != 0 {
			out = append(out, version)
		}
	}
	return out
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

func (s *Server) manageReadAllowed(ctx context.Context) bool {
	scope := principalFromContext(ctx).scope
	access := effectiveManageAccess(scope)
	return access == ScheduleAccessRead || access == ScheduleAccessWrite
}

func (s *Server) manageWriteAllowed(ctx context.Context) bool {
	scope := principalFromContext(ctx).scope
	return effectiveManageAccess(scope) == ScheduleAccessWrite
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

func effectiveManageAccess(scope TokenScope) ScheduleAccess {
	if scope.All {
		return ScheduleAccessWrite
	}
	switch scope.ManageAccess {
	case ScheduleAccessRead, ScheduleAccessWrite:
		return scope.ManageAccess
	default:
		return ScheduleAccessNone
	}
}

func scopeAllowsAgent(scope TokenScope, agentID string) bool {
	if scopeAllowsAllAgents(scope) {
		return !stringListContains(scope.DeniedAgents, agentID)
	}
	if len(scope.Agents) == 0 && len(scope.AgentTags) > 0 {
		return !stringListContains(scope.DeniedAgents, agentID)
	}
	return stringListContains(scope.Agents, agentID) && !stringListContains(scope.DeniedAgents, agentID)
}

func scopeAllowsAgentRecord(scope TokenScope, agent model.Agent) bool {
	if !scopeAllowsAgent(scope, agent.ID) {
		return false
	}
	if len(scope.DeniedTags) > 0 && agentHasAnyLabel(agent, scope.DeniedTags) {
		return false
	}
	if len(scope.AgentTags) == 0 {
		return true
	}
	return agentHasAnyLabel(agent, scope.AgentTags)
}

func scopeAllowsAllAgents(scope TokenScope) bool {
	if scope.All || (len(scope.Agents) == 0 && len(scope.AgentTags) == 0) {
		return true
	}
	for _, allowed := range scope.Agents {
		if allowed == "*" {
			return true
		}
	}
	return false
}

func stringListContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func agentHasAnyLabel(agent model.Agent, labels []string) bool {
	for _, allowed := range labels {
		for _, label := range agent.Labels {
			if label == allowed {
				return true
			}
		}
	}
	return false
}

func permissionArgs(p policy.Policy, toolScope ToolScope, all bool) map[string]string {
	if all || toolScope.AllowedArgs == nil {
		return cloneStringMap(p.AllowedArgs)
	}
	return policy.IntersectAllowedArgs(p.AllowedArgs, toolScope.AllowedArgs)
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

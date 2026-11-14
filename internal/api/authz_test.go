package api

import (
	"context"
	"testing"

	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
)

func TestAuthorizeJobValidatesOmittedHTTPVersionAsHTTP11(t *testing.T) {
	server := &Server{policies: policy.DefaultPolicies()}
	ctx := withPrincipal(context.Background(), principal{scope: TokenScope{
		Tools: map[model.Tool]ToolScope{
			model.ToolHTTP: {AllowedArgs: map[string]string{"method": "HEAD", "http_version": "3"}},
		},
	}})
	err := server.authorizeJob(ctx, model.CreateJobRequest{
		Tool:   model.ToolHTTP,
		Target: "https://example.com",
		Args:   map[string]string{"method": "HEAD"},
	})
	if err == nil {
		t.Fatal("expected omitted HTTP version to be rejected by an HTTP/3-only token")
	}
}

func TestAuthorizeJobDoesNotInferHTTPVersionForMethodOnlyToken(t *testing.T) {
	server := &Server{policies: policy.DefaultPolicies()}
	scope := TokenScope{Tools: map[model.Tool]ToolScope{
		model.ToolHTTP: {AllowedArgs: map[string]string{"method": "HEAD"}},
	}}
	ctx := withPrincipal(context.Background(), principal{scope: scope})
	if err := server.authorizeJob(ctx, model.CreateJobRequest{
		Tool:   model.ToolHTTP,
		Target: "https://example.com",
		Args:   map[string]string{"method": "HEAD"},
	}); err == nil {
		t.Fatal("method-only token must not authorize implicit HTTP/1.1")
	}
	if err := server.authorizeJob(ctx, model.CreateJobRequest{
		Tool:   model.ToolHTTP,
		Target: "https://example.com",
		Args:   map[string]string{"method": "HEAD", "http_version": "2"},
	}); err == nil {
		t.Fatal("expected method-only token to reject HTTP/2")
	}

	permission := permissionsForPolicies(policy.DefaultPolicies(), scope)[model.ToolHTTP]
	if _, ok := permission.AllowedArgs["http_version"]; ok {
		t.Fatalf("method-only token must not infer HTTP/1.1: %#v", permission.AllowedArgs)
	}
}

func TestAuthorizeJobAutoFiltersDeniedDefaultPorts(t *testing.T) {
	server := &Server{policies: policy.DefaultPolicies()}
	ctx := withPrincipal(context.Background(), principal{scope: TokenScope{
		Tools: map[model.Tool]ToolScope{
			model.ToolPing: {
				AllowedArgs: map[string]string{
					"protocol": "icmp,udp,tcp",
					"port":     policy.DefaultUDPProbePort,
				},
			},
		},
	}})
	if err := server.authorizeJob(ctx, model.CreateJobRequest{
		Tool:   model.ToolPing,
		Target: "1.1.1.1",
		Args:   map[string]string{"protocol": "auto"},
	}); err != nil {
		t.Fatalf("expected auto to skip the blocked TCP default port: %v", err)
	}
	if err := server.authorizeJob(ctx, model.CreateJobRequest{
		Tool:   model.ToolPing,
		Target: "1.1.1.1",
		Args:   map[string]string{"protocol": "auto", "port": policy.DefaultUDPProbePort},
	}); err != nil {
		t.Fatalf("expected an explicitly authorized shared auto port to pass: %v", err)
	}
}

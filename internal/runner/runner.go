package runner

import (
	"context"
	"strconv"

	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
)

type Runner struct{}
type StreamSink func(model.StreamEvent) error

func New() Runner {
	return Runner{}
}

func (r Runner) Run(ctx context.Context, job model.Job, p policy.Policy) (*model.ToolResult, error) {
	return r.RunStream(ctx, job, p, nil)
}

func (r Runner) RunStream(ctx context.Context, job model.Job, p policy.Policy, sink StreamSink) (*model.ToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	return runBuiltin(ctx, job, p.ProbeTimeout, sink)
}

func argOr(args map[string]string, key, fallback string) string {
	if args == nil || args[key] == "" {
		return fallback
	}
	return args[key]
}

func parsePositiveInt(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 1
	}
	return n
}

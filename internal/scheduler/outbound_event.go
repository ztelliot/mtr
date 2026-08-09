package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/grpcwire"
)

type eventAgentRequest struct {
	Action string `json:"action"`
	*grpcwire.JobSpec
}

func probeEventAgentHealth(parent context.Context, agent OutboundAgent) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, eventAgentHealthTimeout)
	defer cancel()
	body, err := json.Marshal(eventAgentRequest{
		Action:  "healthz",
		JobSpec: &grpcwire.JobSpec{ProtocolVersion: grpcwire.AgentProtocolVersion},
	})
	if err != nil {
		return nil, errors.New("encode event health request failed")
	}
	response, err := callEventEndpoint(ctx, agent, body, outboundEventHealthHTTPBodyLimit)
	if err != nil {
		return nil, err
	}
	response, err = unwrapEventAgentResponse(response, agent.EventResponse)
	if err != nil {
		return nil, err
	}
	if len(response) > outboundAgentHealthBodyLimit {
		return nil, errors.New("event health response exceeds byte limit")
	}
	return response, nil
}

func callEventAgent(ctx context.Context, agent OutboundAgent, spec *grpcwire.JobSpec, onEvent func(grpcwire.ResultEvent)) error {
	body, err := json.Marshal(eventAgentRequest{Action: "invoke", JobSpec: spec})
	if err != nil {
		return errors.New("encode event invoke request failed")
	}
	response, err := callEventEndpoint(ctx, agent, body, outboundEventResultHTTPBodyLimit)
	if err != nil {
		return err
	}
	response, err = unwrapEventAgentResponse(response, agent.EventResponse)
	if err != nil {
		return err
	}
	events, result, err := decodeEventAgentResult(response, spec.ID, agent.ID)
	if err != nil {
		return err
	}
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		onEvent(event)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	onEvent(result)
	return nil
}

func callEventEndpoint(ctx context.Context, agent OutboundAgent, body []byte, limit int64) ([]byte, error) {
	endpoint, err := url.Parse(agent.URL)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("invalid event endpoint URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("build event request failed")
	}
	// Event invocation is not guaranteed to be idempotent. Prevent the standard
	// transports from replaying this POST even if a configured metadata header
	// happens to look like an idempotency key.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if err := applyOutboundHeaders(req, agent.Headers); err != nil {
		return nil, err
	}
	if err := authorizeOutboundRequest(req, agent.Auth); err != nil {
		return nil, err
	}
	resp, err := outboundHTTPClient(agent).Do(req)
	if err != nil {
		return nil, &outboundAgentConnectionError{err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, errOutboundAgentBusy
		}
		return nil, fmt.Errorf("event endpoint returned HTTP %d", resp.StatusCode)
	}
	return readBoundedOutboundBody(resp.Body, limit)
}

func unwrapEventAgentResponse(body []byte, mode string) ([]byte, error) {
	switch mode {
	case config.EventResponseDirect:
		return body, nil
	case config.EventResponseResultField:
		var wrapper struct {
			Status json.RawMessage `json:"status"`
			Error  json.RawMessage `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if err := unmarshalSingleJSON(body, &wrapper, false); err != nil {
			return nil, errors.New("decode event response wrapper failed")
		}
		if err := parseOuterStatus(wrapper.Status); err != nil {
			return nil, err
		}
		if outerErrorPresent(wrapper.Error) {
			return nil, errors.New("event response wrapper reports an error")
		}
		result := bytes.TrimSpace(wrapper.Result)
		if len(result) == 0 || bytes.Equal(result, []byte("null")) {
			return nil, errors.New("event response wrapper missing result")
		}
		if result[0] == '"' {
			var encoded string
			if err := json.Unmarshal(result, &encoded); err != nil {
				return nil, errors.New("event response result string is invalid")
			}
			result = []byte(encoded)
		}
		result = bytes.TrimSpace(result)
		if len(result) == 0 || result[0] != '{' || len(result) > outboundAgentResultBytesLimit {
			return nil, errors.New("event response result must be a bounded JSON object")
		}
		return result, nil
	default:
		return nil, errors.New("unsupported event response mode")
	}
}

func decodeEventAgentResult(body []byte, jobID string, agentID string) ([]grpcwire.ResultEvent, grpcwire.ResultEvent, error) {
	if len(body) > outboundAgentResultBytesLimit {
		return nil, grpcwire.ResultEvent{}, errors.New("event result exceeds byte limit")
	}
	var fields map[string]json.RawMessage
	if err := unmarshalSingleJSON(body, &fields, false); err != nil {
		return nil, grpcwire.ResultEvent{}, errors.New("decode event result failed")
	}
	if len(fields) != 3 {
		return nil, grpcwire.ResultEvent{}, errors.New("event result must contain only job_id, events, and result")
	}
	for _, key := range []string{"job_id", "events", "result"} {
		if _, ok := fields[key]; !ok {
			return nil, grpcwire.ResultEvent{}, errors.New("event result is missing a required field")
		}
	}
	var returnedJobID string
	if err := json.Unmarshal(fields["job_id"], &returnedJobID); err != nil || returnedJobID == "" || returnedJobID != jobID {
		return nil, grpcwire.ResultEvent{}, errors.New("event result job_id does not match the request")
	}
	var rawEvents []json.RawMessage
	if err := json.Unmarshal(fields["events"], &rawEvents); err != nil || rawEvents == nil {
		return nil, grpcwire.ResultEvent{}, errors.New("event result events must be an array")
	}
	if len(rawEvents) > outboundAgentResultEventLimit {
		return nil, grpcwire.ResultEvent{}, errors.New("event result exceeds event limit")
	}
	events := make([]grpcwire.ResultEvent, 0, len(rawEvents))
	for _, raw := range rawEvents {
		event, err := decodeEventAgentPayload(raw, jobID, agentID, false)
		if err != nil {
			return nil, grpcwire.ResultEvent{}, err
		}
		events = append(events, event)
	}
	result, err := decodeEventAgentPayload(fields["result"], jobID, agentID, true)
	if err != nil {
		return nil, grpcwire.ResultEvent{}, err
	}
	return events, result, nil
}

func decodeEventAgentPayload(raw []byte, jobID string, agentID string, summary bool) (grpcwire.ResultEvent, error) {
	var payload map[string]any
	if err := unmarshalSingleJSON(raw, &payload, false); err != nil || payload == nil {
		return grpcwire.ResultEvent{}, errors.New("event payload must be a JSON object")
	}
	typeName, ok := payload["type"].(string)
	if !ok || typeName == "" || typeName != strings.TrimSpace(typeName) {
		return grpcwire.ResultEvent{}, errors.New("event payload type is required")
	}
	if summary && typeName != "summary" {
		return grpcwire.ResultEvent{}, errors.New("event result must be a summary")
	}
	if !summary && typeName == "summary" {
		return grpcwire.ResultEvent{}, errors.New("event progress list must not contain a summary")
	}
	if summary {
		exitCode, ok := payload["exit_code"].(float64)
		if !ok || math.Trunc(exitCode) != exitCode {
			return grpcwire.ResultEvent{}, errors.New("event summary exit_code must be an integer")
		}
	}
	return grpcwire.ResultEvent{JobID: jobID, AgentID: agentID, Event: payload}, nil
}

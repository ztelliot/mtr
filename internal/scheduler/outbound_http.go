package scheduler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/ztelliot/mtr/internal/grpcwire"
)

func probeHTTPTransportHealth(parent context.Context, agent OutboundAgent) ([]byte, error) {
	healthURL, err := httpTransportURL(agent.URL, "/healthz")
	if err != nil {
		return nil, errors.New("build outbound health request failed")
	}
	ctx, cancel := context.WithTimeout(parent, httpTransportHealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return nil, errors.New("build outbound health request failed")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
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
		return nil, fmt.Errorf("outbound health returned HTTP %d", resp.StatusCode)
	}
	return readBoundedOutboundBody(resp.Body, outboundAgentHealthBodyLimit)
}

func callHTTPTransportOnce(ctx context.Context, agent OutboundAgent, spec *grpcwire.JobSpec, onEvent func(grpcwire.ResultEvent)) error {
	body, err := json.Marshal(spec)
	if err != nil {
		return errors.New("encode outbound job failed")
	}
	invokeURL, err := httpTransportURL(agent.URL, "/invoke")
	if err != nil {
		return errors.New("build outbound invoke request failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, invokeURL, bytes.NewReader(body))
	if err != nil {
		return errors.New("build outbound invoke request failed")
	}
	// The standard transports may otherwise replay a POST after certain
	// connection or HTTP/2 stream failures because bytes.Reader supplies
	// GetBody. The Agent protocol has no end-to-end idempotency guarantee.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	if err := applyOutboundHeaders(req, agent.Headers); err != nil {
		return err
	}
	if err := authorizeOutboundRequest(req, agent.Auth); err != nil {
		return err
	}
	var requestMayHaveStarted atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			// GotConn is the last synchronous standard-Transport trace point
			// before request I/O. Once reached, conservatively assume the Agent
			// may receive the request and never replay a later connection error.
			requestMayHaveStarted.Store(true)
		},
	}))
	client := outboundHTTPClient(agent)
	requestStartStateKnown := outboundHTTPClientSupportsGotConnTrace(client)
	resp, err := client.Do(req)
	if err != nil {
		return &outboundAgentConnectionError{
			err:                    err,
			requestMayHaveStarted:  requestMayHaveStarted.Load(),
			requestStartStateKnown: requestStartStateKnown,
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusTooManyRequests {
			return errOutboundAgentBusy
		}
		return fmt.Errorf("outbound invoke returned HTTP %d", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	eventCount := 0
	totalBytes := 0
	for {
		line, readErr := readOutboundAgentNDJSONLine(reader)
		totalBytes += len(line)
		if totalBytes > outboundAgentResultBytesLimit {
			return errors.New("outbound result exceeds byte limit")
		}
		if len(line) > 0 {
			if line[len(line)-1] == '\n' {
				line = line[:len(line)-1]
			}
			if len(line) == 0 || !bytes.Equal(line, bytes.TrimSpace(line)) {
				return errors.New("outbound result contains a non-canonical event line")
			}
			event, err := decodeOutboundAgentLine(line, spec.ID, agent.ID)
			if err != nil {
				return err
			}
			if eventType(event.Event) != "summary" {
				eventCount++
				if eventCount > outboundAgentResultEventLimit {
					return errors.New("outbound result exceeds event limit")
				}
			}
			onEvent(event)
			if eventType(event.Event) == "summary" {
				return nil
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return errors.New("outbound invoke ended before summary")
			}
			return &outboundAgentConnectionError{err: readErr, requestMayHaveStarted: true, requestStartStateKnown: true}
		}
	}
}

func outboundHTTPClientSupportsGotConnTrace(client *http.Client) bool {
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	_, ok := transport.(*http.Transport)
	return ok
}

func readOutboundAgentNDJSONLine(reader *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		part, err := reader.ReadSlice('\n')
		out = append(out, part...)
		if len(out) > httpTransportNDJSONLineLimit {
			return nil, fmt.Errorf("outbound event line exceeds %d bytes", httpTransportNDJSONLineLimit)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return out, err
	}
}

func httpTransportURL(baseURL string, path string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid outbound HTTP URL")
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawPath = ""
	return u.String(), nil
}

func decodeOutboundAgentLine(line []byte, jobID string, agentID string) (grpcwire.ResultEvent, error) {
	var event map[string]any
	if err := unmarshalSingleJSON(line, &event, false); err != nil {
		return grpcwire.ResultEvent{}, errors.New("decode outbound event failed")
	}
	typeName, ok := event["type"].(string)
	if !ok || typeName == "" || typeName != strings.TrimSpace(typeName) {
		return grpcwire.ResultEvent{}, errors.New("outbound event missing type")
	}
	return grpcwire.ResultEvent{JobID: jobID, AgentID: agentID, Event: event}, nil
}

func readBoundedOutboundBody(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, &outboundAgentConnectionError{err: err}
	}
	if int64(len(body)) > limit {
		return nil, errors.New("outbound response exceeds byte limit")
	}
	return body, nil
}

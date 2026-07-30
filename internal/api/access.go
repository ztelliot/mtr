package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	accessAuthPublic        = "public"
	accessAuthNotEvaluated  = "not_evaluated"
	accessAuthAnonymous     = "anonymous"
	accessAuthAuthenticated = "authenticated"
	accessAuthMissing       = "missing"
	accessAuthMalformed     = "malformed"
	accessAuthInvalid       = "invalid"
)

type httpAccessContextKey struct{}

type httpAccessContext struct {
	authResult string
	subject    string
}

type httpAccessMetrics struct {
	requests     *prometheus.CounterVec
	duration     *prometheus.HistogramVec
	requestSize  *prometheus.HistogramVec
	responseSize *prometheus.HistogramVec
	inFlight     *prometheus.GaugeVec
}

func newHTTPAccessMetrics() *httpAccessMetrics {
	sizeBuckets := []float64{128, 512, 1024, 4 * 1024, 16 * 1024, 64 * 1024, 256 * 1024, 1024 * 1024, 4 * 1024 * 1024, 16 * 1024 * 1024}
	return &httpAccessMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mtr_http_requests_total",
			Help: "Total HTTP requests handled by method, route, status code, and authentication result.",
		}, []string{"method", "route", "status_code", "auth_result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mtr_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds by method and route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		requestSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mtr_http_request_size_bytes",
			Help:    "HTTP request body bytes read by method and route.",
			Buckets: sizeBuckets,
		}, []string{"method", "route"}),
		responseSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mtr_http_response_size_bytes",
			Help:    "HTTP response body bytes written by method and route.",
			Buckets: sizeBuckets,
		}, []string{"method", "route"}),
		inFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mtr_http_requests_in_flight",
			Help: "Current number of HTTP requests being served by method.",
		}, []string{"method"}),
	}
}

func (m *httpAccessMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{m.requests, m.duration, m.requestSize, m.responseSize, m.inFlight}
}

func (m *httpAccessMetrics) observe(method, route string, status int, authResult string, duration time.Duration, requestBytes, responseBytes int64) {
	if m == nil {
		return
	}
	method = metricHTTPMethod(method)
	route = metricHTTPRoute(route)
	authResult = metricHTTPAuthResult(authResult)
	m.requests.WithLabelValues(method, route, strconv.Itoa(status), authResult).Inc()
	m.duration.WithLabelValues(method, route).Observe(duration.Seconds())
	m.requestSize.WithLabelValues(method, route).Observe(float64(requestBytes))
	m.responseSize.WithLabelValues(method, route).Observe(float64(responseBytes))
}

func (m *httpAccessMetrics) begin(method string) func() {
	if m == nil {
		return func() {}
	}
	gauge := m.inFlight.WithLabelValues(metricHTTPMethod(method))
	gauge.Inc()
	return gauge.Dec
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := normalizedRequestID(r.Header.Get("X-Request-ID"))
		w.Header().Set("X-Request-ID", requestID)

		access := &httpAccessContext{authResult: accessAuthPublic}
		r = r.WithContext(context.WithValue(r.Context(), httpAccessContextKey{}, access))
		body := &countingReadCloser{ReadCloser: r.Body}
		if r.Body != nil {
			r.Body = body
		}
		wrapped := chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor)
		finishInFlight := s.accessMetrics.begin(r.Method)
		panicked := true
		defer func() {
			finishInFlight()
			status := wrapped.Status()
			if status == 0 {
				if panicked {
					status = http.StatusInternalServerError
				} else {
					status = http.StatusOK
				}
			}
			duration := time.Since(started)
			route := routePattern(r)
			requestBytes := body.BytesRead()
			responseBytes := int64(wrapped.BytesWritten())
			contextError := ""
			if err := r.Context().Err(); err != nil {
				contextError = err.Error()
			}
			s.accessMetrics.observe(r.Method, route, status, access.authResult, duration, requestBytes, responseBytes)
			s.log.LogAttrs(r.Context(), slog.LevelInfo, "http access",
				slog.String("request_id", requestID),
				slog.String("method", r.Method),
				slog.String("route", route),
				slog.String("path", r.URL.EscapedPath()),
				slog.Int("status", status),
				slog.Float64("duration_ms", float64(duration.Microseconds())/1000),
				slog.Int64("request_bytes", requestBytes),
				slog.Int64("response_bytes", responseBytes),
				slog.Int64("content_length", r.ContentLength),
				slog.String("content_type", r.Header.Get("Content-Type")),
				slog.String("response_content_type", wrapped.Header().Get("Content-Type")),
				slog.String("client_ip", s.clientIP.Resolve(r)),
				slog.String("remote_addr", r.RemoteAddr),
				slog.String("host", r.Host),
				slog.String("scheme", requestScheme(r)),
				slog.String("protocol", r.Proto),
				slog.String("user_agent", r.UserAgent()),
				slog.Bool("query_present", r.URL.RawQuery != ""),
				slog.String("auth_result", metricHTTPAuthResult(access.authResult)),
				slog.String("subject", access.subject),
				slog.String("context_error", contextError),
				slog.Bool("panicked", panicked),
			)
		}()

		next.ServeHTTP(wrapped, r)
		panicked = false
	})
}

func setHTTPAccessAuthentication(ctx context.Context, result, subject string) {
	access, _ := ctx.Value(httpAccessContextKey{}).(*httpAccessContext)
	if access == nil {
		return
	}
	access.authResult = metricHTTPAuthResult(result)
	access.subject = subject
}

type countingReadCloser struct {
	io.ReadCloser
	bytesRead int64
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	if r.ReadCloser == nil {
		return 0, io.EOF
	}
	n, err := r.ReadCloser.Read(p)
	r.bytesRead += int64(n)
	return n, err
}

func (r *countingReadCloser) BytesRead() int64 {
	if r == nil {
		return 0
	}
	return r.bytesRead
}

func routePattern(r *http.Request) string {
	pattern := strings.TrimSpace(chi.RouteContext(r.Context()).RoutePattern())
	if pattern == "" {
		return "unmatched"
	}
	return pattern
}

func metricHTTPRoute(route string) string {
	route = strings.TrimSpace(route)
	if route == "" {
		return "unmatched"
	}
	return route
}

func metricHTTPMethod(method string) string {
	switch method = strings.ToUpper(strings.TrimSpace(method)); method {
	case http.MethodConnect, http.MethodDelete, http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPatch, http.MethodPost, http.MethodPut, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}

func metricHTTPAuthResult(result string) string {
	switch result {
	case accessAuthPublic, accessAuthNotEvaluated, accessAuthAnonymous, accessAuthAuthenticated, accessAuthMissing, accessAuthMalformed, accessAuthInvalid:
		return result
	default:
		return accessAuthNotEvaluated
	}
}

func normalizedRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return uuid.NewString()
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-._:/", r) {
			continue
		}
		return uuid.NewString()
	}
	return value
}

func requestScheme(r *http.Request) string {
	if r != nil && r.TLS != nil {
		return "https"
	}
	return "http"
}

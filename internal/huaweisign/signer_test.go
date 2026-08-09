package huaweisign

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAccessKey = "AKIDEXAMPLE123456789"
	testSecretKey = "secret-example-key"
	testProjectID = "0123456789abcdef0123456789abcdef"
)

var testSigningTime = time.Date(2026, time.August, 9, 1, 2, 3, 0, time.UTC)

// This vector was generated with Huawei Cloud's official APIG Go signing SDK
// 2.0.3. Host was explicitly included as prescribed by the FunctionGraph API
// signing documentation.
func TestSignerMatchesOfficialGoSDKVector(t *testing.T) {
	signer, err := newSigner(testAccessKey, testSecretKey, testProjectID, func() time.Time { return testSigningTime })
	if err != nil {
		t.Fatal(err)
	}
	requestURL := "https://functiongraph.cn-east-3.myhuaweicloud.com/v2/" + testProjectID +
		"/fgs/functions/urn:fss:cn-east-3:" + testProjectID +
		":function:default:mtr-agent:latest/invocations?z=last&a=hello%20world&a=%2F"
	body := []byte(`{"action":"invoke","target":"example.com"}`)
	req, err := http.NewRequest(http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	// The outbound transports disable automatic POST replay before signing.
	req.GetBody = nil
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CFF-Request-Version", "v1")

	if err := signer.Sign(req); err != nil {
		t.Fatal(err)
	}
	const wantAuthorization = "SDK-HMAC-SHA256 Access=AKIDEXAMPLE123456789, SignedHeaders=accept;content-type;host;x-cff-request-version;x-project-id;x-sdk-date, Signature=34b0137e5fe90dfb50dae6148d26b48a5f0097fe9cae9130ea1cdc546900bea3"
	if got := req.Header.Get(authorizationHeader); got != wantAuthorization {
		t.Fatalf("Authorization = %q, want %q", got, wantAuthorization)
	}
	if got := req.Header.Get(dateHeader); got != "20260809T010203Z" {
		t.Fatalf("X-Sdk-Date = %q", got)
	}
	if got := req.Header.Get(projectIDHeader); got != testProjectID {
		t.Fatalf("X-Project-Id = %q", got)
	}
	if got := req.URL.RawQuery; got != "a=%2F&a=hello%20world&z=last" {
		t.Fatalf("canonical query = %q", got)
	}
	if req.GetBody != nil {
		t.Fatal("signing made the request replayable")
	}
	gotBody, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("body = %q, want %q", gotBody, body)
	}
}

func TestSignerCoversMethodURIHeadersAndBody(t *testing.T) {
	signer, err := newSigner(testAccessKey, testSecretKey, testProjectID, func() time.Time { return testSigningTime })
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		method string
		url    string
		header string
		body   string
	}{
		{name: "baseline", method: http.MethodPost, url: "https://functiongraph.example/v2/p/fgs/functions/urn:fss:r:p:function:d:f:v/invocations", header: "v1", body: `{"action":"healthz"}`},
		{name: "method", method: http.MethodPut, url: "https://functiongraph.example/v2/p/fgs/functions/urn:fss:r:p:function:d:f:v/invocations", header: "v1", body: `{"action":"healthz"}`},
		{name: "urn", method: http.MethodPost, url: "https://functiongraph.example/v2/p/fgs/functions/urn:fss:r:p:function:d:other:v/invocations", header: "v1", body: `{"action":"healthz"}`},
		{name: "query", method: http.MethodPost, url: "https://functiongraph.example/v2/p/fgs/functions/urn:fss:r:p:function:d:f:v/invocations?a=1", header: "v1", body: `{"action":"healthz"}`},
		{name: "header", method: http.MethodPost, url: "https://functiongraph.example/v2/p/fgs/functions/urn:fss:r:p:function:d:f:v/invocations", header: "v2", body: `{"action":"healthz"}`},
		{name: "body", method: http.MethodPost, url: "https://functiongraph.example/v2/p/fgs/functions/urn:fss:r:p:function:d:f:v/invocations", header: "v1", body: `{"action":"invoke"}`},
	}
	signatures := make(map[string]string, len(tests))
	for _, test := range tests {
		req, err := http.NewRequest(test.method, test.url, strings.NewReader(test.body))
		if err != nil {
			t.Fatal(err)
		}
		req.GetBody = nil
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CFF-Request-Version", test.header)
		if err := signer.Sign(req); err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		authorization := req.Header.Get(authorizationHeader)
		if authorization == "" {
			t.Fatalf("%s: missing authorization", test.name)
		}
		if previous, duplicate := signatures[authorization]; duplicate {
			t.Fatalf("%s and %s produced the same signature", previous, test.name)
		}
		signatures[authorization] = test.name
	}
}

func TestSignerIsSafeForConcurrentRequests(t *testing.T) {
	signer, err := newSigner(testAccessKey, testSecretKey, testProjectID, func() time.Time { return testSigningTime })
	if err != nil {
		t.Fatal(err)
	}
	const requestCount = 128
	var wait sync.WaitGroup
	errors := make(chan error, requestCount)
	authorizations := make(chan string, requestCount)
	for index := range requestCount {
		wait.Add(1)
		go func() {
			defer wait.Done()
			req, err := http.NewRequest(http.MethodPost, "https://functiongraph.example/invoke", strings.NewReader(string(rune(index))))
			if err != nil {
				errors <- err
				return
			}
			req.GetBody = nil
			req.Header.Set("Content-Type", "application/json")
			if err := signer.Sign(req); err != nil {
				errors <- err
				return
			}
			if req.GetBody != nil {
				errors <- io.ErrUnexpectedEOF
				return
			}
			authorizations <- req.Header.Get(authorizationHeader)
		}()
	}
	wait.Wait()
	close(errors)
	close(authorizations)
	for err := range errors {
		t.Fatal(err)
	}
	seen := make(map[string]struct{}, requestCount)
	for authorization := range authorizations {
		if authorization == "" {
			t.Fatal("missing authorization")
		}
		seen[authorization] = struct{}{}
	}
	if len(seen) != requestCount {
		t.Fatalf("unique signatures = %d, want %d", len(seen), requestCount)
	}
}

func TestSignerRejectsMalformedQuery(t *testing.T) {
	signer, err := newSigner(testAccessKey, testSecretKey, testProjectID, func() time.Time { return testSigningTime })
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://functiongraph.example/invoke", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.URL.RawQuery = "invalid;query"
	if err := signer.Sign(req); err == nil {
		t.Fatal("malformed query was accepted")
	}
}

func TestSignerRestoresBodyWithoutEnablingReplay(t *testing.T) {
	signer, err := newSigner(testAccessKey, testSecretKey, testProjectID, func() time.Time { return testSigningTime })
	if err != nil {
		t.Fatal(err)
	}
	const payload = `{"action":"invoke"}`
	body := &trackingReadCloser{Reader: strings.NewReader(payload)}
	req, err := http.NewRequest(http.MethodPost, "https://functiongraph.example/invoke", body)
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = nil
	req.ContentLength = int64(len(payload))
	originalLength := req.ContentLength
	if err := signer.Sign(req); err != nil {
		t.Fatal(err)
	}
	if !body.closed {
		t.Fatal("original request body was not closed")
	}
	if req.GetBody != nil {
		t.Fatal("signing enabled automatic request replay")
	}
	if req.ContentLength != originalLength {
		t.Fatalf("ContentLength = %d, want %d", req.ContentLength, originalLength)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(restored), payload; got != want {
		t.Fatalf("restored body = %q, want %q", got, want)
	}
}

func TestNewRejectsInvalidCredentials(t *testing.T) {
	for name, credentials := range map[string][3]string{
		"access key":  {"", testSecretKey, testProjectID},
		"secret key":  {testAccessKey, "", testProjectID},
		"project id":  {testAccessKey, testSecretKey, ""},
		"auth syntax": {"access,key", testSecretKey, testProjectID},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newSigner(credentials[0], credentials[1], credentials[2], time.Now); err == nil {
				t.Fatal("invalid credentials were accepted")
			}
		})
	}
	if _, err := newSigner(testAccessKey, testSecretKey, testProjectID, nil); err == nil {
		t.Fatal("nil clock was accepted")
	}
}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (body *trackingReadCloser) Close() error {
	body.closed = true
	return nil
}

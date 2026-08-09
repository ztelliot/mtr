// Package huaweisign signs Huawei Cloud API requests with SDK-HMAC-SHA256.
package huaweisign

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"golang.org/x/net/http/httpguts"
)

const (
	algorithm           = "SDK-HMAC-SHA256"
	authorizationHeader = "Authorization"
	dateHeader          = "X-Sdk-Date"
	dateFormat          = "20060102T150405Z"
	projectIDHeader     = "X-Project-Id"
)

// Signer is safe for concurrent use when the supplied clock is safe for
// concurrent use. New uses time.Now, which is safe for concurrent use.
type Signer struct {
	accessKey string
	secretKey string
	projectID string
	now       func() time.Time
}

// New constructs a signer for a project-scoped Huawei Cloud credential.
func New(accessKey, secretKey, projectID string) (*Signer, error) {
	return newSigner(accessKey, secretKey, projectID, time.Now)
}

func newSigner(accessKey, secretKey, projectID string, now func() time.Time) (*Signer, error) {
	if !config.ValidHuaweiCloudAccessKey(accessKey) {
		return nil, errors.New("invalid Huawei Cloud access key")
	}
	if secretKey == "" {
		return nil, errors.New("invalid Huawei Cloud secret key")
	}
	if projectID == "" || !httpguts.ValidHeaderFieldValue(projectID) {
		return nil, errors.New("invalid Huawei Cloud project ID")
	}
	if now == nil {
		return nil, errors.New("Huawei Cloud signing clock is required")
	}
	return &Signer{accessKey: accessKey, secretKey: secretKey, projectID: projectID, now: now}, nil
}

// Sign adds the project, timestamp, and Authorization headers to req. The
// signature covers the request's final method, URL, headers, and body. Sign
// must therefore be the last request-building operation before transmission.
func (s *Signer) Sign(req *http.Request) error {
	if s == nil || req == nil || req.URL == nil || req.Method == "" {
		return errors.New("invalid request for Huawei Cloud signing")
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	if host == "" || !httpguts.ValidHostHeader(host) {
		return errors.New("invalid request host for Huawei Cloud signing")
	}

	payloadHash, err := hashRequestPayload(req)
	if err != nil {
		return err
	}
	query, err := canonicalQuery(req.URL.RawQuery)
	if err != nil {
		return err
	}
	// The canonical query must also be the query transmitted on the wire.
	req.URL.RawQuery = query

	removeHeaderFold(req.Header, authorizationHeader)
	removeHeaderFold(req.Header, projectIDHeader)
	removeHeaderFold(req.Header, dateHeader)
	req.Header.Set(projectIDHeader, s.projectID)
	timestamp := s.now().UTC().Format(dateFormat)
	req.Header.Set(dateHeader, timestamp)

	headerBlock, signedHeaders, err := canonicalHeaders(req, host)
	if err != nil {
		return err
	}
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.Path),
		query,
		headerBlock,
		strings.Join(signedHeaders, ";"),
		payloadHash,
	}, "\n")
	canonicalDigest := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := algorithm + "\n" + timestamp + "\n" + hex.EncodeToString(canonicalDigest[:])
	mac := hmac.New(sha256.New, []byte(s.secretKey))
	_, _ = mac.Write([]byte(stringToSign))
	signature := hex.EncodeToString(mac.Sum(nil))
	req.Header.Set(authorizationHeader, fmt.Sprintf(
		"%s Access=%s, SignedHeaders=%s, Signature=%s",
		algorithm, s.accessKey, strings.Join(signedHeaders, ";"), signature,
	))
	return nil
}

func hashRequestPayload(req *http.Request) (string, error) {
	var payload []byte
	if req.Body != nil {
		original := req.Body
		var err error
		payload, err = io.ReadAll(original)
		if err != nil {
			_ = original.Close()
			return "", errors.New("read request body for Huawei Cloud signing failed")
		}
		// Restore only Body. In particular, do not populate GetBody: outbound
		// Agent POSTs deliberately disable transport-level replay.
		req.Body = io.NopCloser(bytes.NewReader(payload))
		if err := original.Close(); err != nil {
			return "", errors.New("close request body after Huawei Cloud signing failed")
		}
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func removeHeaderFold(header http.Header, target string) {
	for name := range header {
		if strings.EqualFold(name, target) {
			delete(header, name)
		}
	}
}

func canonicalURI(path string) string {
	segments := strings.Split(path, "/")
	for index := range segments {
		segments[index] = escape(segments[index])
	}
	canonical := strings.Join(segments, "/")
	if !strings.HasSuffix(canonical, "/") {
		canonical += "/"
	}
	return canonical
}

func canonicalQuery(rawQuery string) (string, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", errors.New("invalid query for Huawei Cloud signing")
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(values))
	for _, key := range keys {
		items := values[key]
		sort.Strings(items)
		for _, value := range items {
			parts = append(parts, escape(key)+"="+escape(value))
		}
	}
	return strings.Join(parts, "&"), nil
}

func canonicalHeaders(req *http.Request, host string) (string, []string, error) {
	values := make(map[string][]string, len(req.Header)+1)
	for name, headerValues := range req.Header {
		lowerName := strings.ToLower(name)
		if lowerName == strings.ToLower(authorizationHeader) {
			continue
		}
		if !httpguts.ValidHeaderFieldName(lowerName) || len(headerValues) == 0 {
			return "", nil, errors.New("invalid header for Huawei Cloud signing")
		}
		for _, value := range headerValues {
			if !httpguts.ValidHeaderFieldValue(value) {
				return "", nil, errors.New("invalid header for Huawei Cloud signing")
			}
			values[lowerName] = append(values[lowerName], value)
		}
	}
	values["host"] = []string{host}

	signedHeaders := make([]string, 0, len(values))
	for name := range values {
		signedHeaders = append(signedHeaders, name)
	}
	sort.Strings(signedHeaders)

	lines := make([]string, 0, len(values))
	for _, name := range signedHeaders {
		headerValues := values[name]
		sort.Strings(headerValues)
		for _, value := range headerValues {
			lines = append(lines, name+":"+strings.TrimSpace(value))
		}
	}
	return strings.Join(lines, "\n") + "\n", signedHeaders, nil
}

func escape(value string) string {
	const upperHex = "0123456789ABCDEF"
	var escaped strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'A' && character <= 'Z') ||
			(character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' || character == '~' {
			escaped.WriteByte(character)
			continue
		}
		escaped.WriteByte('%')
		escaped.WriteByte(upperHex[character>>4])
		escaped.WriteByte(upperHex[character&0x0f])
	}
	return escaped.String()
}

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

var geoIPHTTPClient = http.DefaultClient

type geoIPUpstreamResponse struct {
	ASN struct {
		Number       int    `json:"number"`
		Organization string `json:"organization"`
	} `json:"asn"`
	Country struct {
		Name string `json:"name"`
	} `json:"country"`
	Region  string `json:"region"`
	City    string `json:"city"`
	ISP     string `json:"isp"`
	Reverse string `json:"reverse"`
}

type geoIPResponse struct {
	Reverse string `json:"reverse"`
	Country string `json:"country"`
	Region  string `json:"region"`
	City    string `json:"city"`
	ISP     string `json:"isp"`
	ASN     int    `json:"asn"`
	Org     string `json:"org"`
}

func (s *Server) getGeoIP(w http.ResponseWriter, r *http.Request) {
	if !s.limiterSnapshot().AllowGeoIP() {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	rawIP, err := geoIPParamIP(chi.URLParam(r, "ip"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ip")
		return
	}
	upstreamURL, err := s.geoIPURLForIP(rawIP)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp, err := geoIPHTTPClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("geoip upstream returned HTTP %d", resp.StatusCode))
		return
	}
	var upstream geoIPUpstreamResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, geoIPUpstreamBodyLimit+geoIPUpstreamBodyLimitPad)).Decode(&upstream); err != nil {
		writeError(w, http.StatusBadGateway, "invalid geoip response")
		return
	}
	writeJSON(w, http.StatusOK, geoIPResponse{
		Reverse: upstream.Reverse,
		Country: upstream.Country.Name,
		Region:  upstream.Region,
		City:    upstream.City,
		ISP:     upstream.ISP,
		ASN:     upstream.ASN.Number,
		Org:     upstream.ASN.Organization,
	})
}

func geoIPParamIP(raw string) (string, error) {
	decoded, err := url.PathUnescape(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(strings.TrimSpace(decoded))
	if ip == nil {
		return "", errors.New("invalid ip")
	}
	return ip.String(), nil
}

func (s *Server) geoIPURLForIP(ip string) (string, error) {
	base, err := url.Parse(s.geoIPURL)
	if err != nil {
		return "", err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + url.PathEscape(ip)
	base.RawQuery = ""
	base.Fragment = ""
	return base.String(), nil
}

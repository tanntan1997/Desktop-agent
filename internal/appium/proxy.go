// Package appium forwards WebDriver HTTP calls to a local Appium server, so a
// remote test runner can drive devices through the tunnel without network
// access to this machine.
package appium

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/ababank/mobile-device-agent/internal/protocol"
)

// Request is a single forwarded HTTP call.
type Request struct {
	Method string          `json:"method"`
	Path   string          `json:"path"` // e.g. /session, /session/{id}/element
	Body   json.RawMessage `json:"body,omitempty"`
}

// Response carries the upstream status and body. JSON bodies are embedded as
// JSON; anything else is returned as text.
type Response struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
	Text   string          `json:"text,omitempty"`
}

type Proxy struct {
	base   *url.URL
	client *http.Client
}

func New(rawURL string) (*Proxy, error) {
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid appium url %q", rawURL)
	}
	// Session creation can take minutes (app install, WDA build).
	return &Proxy{base: u, client: &http.Client{Timeout: 10 * time.Minute}}, nil
}

func (p *Proxy) URL() string { return p.base.String() }

func (p *Proxy) Do(ctx context.Context, r Request) (Response, error) {
	status, body, err := Forward(ctx, p.client, p.base.String(), r)
	if err != nil {
		return Response{}, protocol.Errorf(protocol.CodeUnavailable, "appium at %s: %v", p.base, err)
	}
	return BuildResponse(status, body), nil
}

// Forward sends r to baseURL+r.Path. The path is appended to a fixed base, so
// the request cannot be redirected to another host.
func Forward(ctx context.Context, client *http.Client, baseURL string, r Request) (int, []byte, error) {
	if !strings.HasPrefix(r.Path, "/") {
		return 0, nil, protocol.Errorf(protocol.CodeBadRequest, "path must start with /")
	}
	method := strings.ToUpper(r.Method)
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+r.Path, body)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	return resp.StatusCode, b, err
}

func BuildResponse(status int, body []byte) Response {
	if json.Valid(body) && len(bytes.TrimSpace(body)) > 0 {
		return Response{Status: status, Body: body}
	}
	return Response{Status: status, Text: string(body)}
}

// ReverseProxy exposes Appium under prefix on the local HTTP API.
func (p *Proxy) ReverseProxy(prefix string) http.Handler {
	rp := httputil.NewSingleHostReverseProxy(p.base)
	return http.StripPrefix(prefix, rp)
}

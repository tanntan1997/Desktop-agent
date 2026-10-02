// Package server is the web server QA tooling and browsers talk to. It
// accepts agent tunnels on /agent and exposes the connected agents and their
// devices over a token-protected REST API, an SSE event stream, and a small
// embedded dashboard.
package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ababank/mobile-device-agent/internal/autotest"
	"github.com/ababank/mobile-device-agent/internal/hub"
	"github.com/ababank/mobile-device-agent/internal/protocol"
)

//go:embed web
var webFS embed.FS

const defaultTimeout = 2 * time.Minute

type Server struct {
	Hub *hub.Hub
	// APITokens are accepted on /api/*. Empty disables API auth.
	APITokens      []string
	AllowedOrigins []string
	// AutoTest enables AI-driven tests (/api/tests, /api/runs). Optional.
	AutoTest *autotest.Service
	Log      *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /agent", s.Hub)

	web, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServerFS(web))

	api := http.NewServeMux()
	api.HandleFunc("GET /api/events", s.handleEvents)
	api.HandleFunc("GET /api/agents", s.handleAgents)
	api.HandleFunc("GET /api/agents/{agent}", s.handleAgent)
	api.HandleFunc("POST /api/agents/{agent}/rpc", s.handleRPC)
	api.HandleFunc("GET /api/devices", s.handleDevices)

	const dev = "/api/agents/{agent}/devices/{serial}"
	api.HandleFunc("GET "+dev, s.call("device.info", queryParams("serial")))
	api.HandleFunc("GET "+dev+"/screenshot", s.handleScreenshot)
	api.HandleFunc("GET "+dev+"/source", s.handleSource)
	api.HandleFunc("POST "+dev+"/tap", s.call("device.tap", bodyParams("serial")))
	api.HandleFunc("POST "+dev+"/swipe", s.call("device.swipe", bodyParams("serial")))
	api.HandleFunc("POST "+dev+"/text", s.call("device.text", bodyParams("serial")))
	api.HandleFunc("POST "+dev+"/key", s.call("device.key", bodyParams("serial")))
	api.HandleFunc("POST "+dev+"/apps/install", s.call("app.install", bodyParams("serial")))
	api.HandleFunc("POST "+dev+"/apps/launch", s.call("app.launch", bodyParams("serial")))
	api.HandleFunc("POST "+dev+"/apps/terminate", s.call("app.terminate", bodyParams("serial")))

	const jobs = "/api/agents/{agent}/jobs"
	api.HandleFunc("GET "+jobs, s.call("job.list", nil))
	api.HandleFunc("POST "+jobs, s.call("job.start", bodyParams()))
	api.HandleFunc("GET "+jobs+"/{id}", s.call("job.get", queryParams("id")))
	api.HandleFunc("POST "+jobs+"/{id}/cancel", s.call("job.cancel", queryParams("id")))
	api.HandleFunc("GET "+jobs+"/{id}/logs", s.call("job.logs", queryParams("id")))
	api.HandleFunc("GET "+jobs+"/{id}/artifacts", s.call("job.artifacts", queryParams("id")))
	api.HandleFunc("GET "+jobs+"/{id}/artifacts/{name...}", s.handleArtifact)

	if s.AutoTest != nil {
		s.autotestRoutes(api)
	}

	mux.Handle("/api/", s.auth(api))
	return s.cors(mux)
}

// auth accepts "Authorization: Bearer <token>" or "?token=" (for <img src>
// and EventSource, which cannot set headers).
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.APITokens) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		for _, want := range s.APITokens {
			if subtle.ConstantTimeCompare([]byte(tok), []byte(want)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}
		writeErr(w, protocol.Errorf(protocol.CodeForbidden, "missing or invalid API token"))
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (slices.Contains(s.AllowedOrigins, origin) || slices.Contains(s.AllowedOrigins, "*")) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) agent(r *http.Request) (*hub.Agent, error) {
	id := r.PathValue("agent")
	if a := s.Hub.Get(id); a != nil {
		return a, nil
	}
	return nil, protocol.Errorf(protocol.CodeNotFound, "agent %q is not connected", id)
}

// invoke calls method on the agent named in the path, bounded by ?timeoutSec=
// (default 2m) and the client's connection.
func (s *Server) invoke(r *http.Request, method string, params json.RawMessage) (json.RawMessage, error) {
	a, err := s.agent(r)
	if err != nil {
		return nil, err
	}
	timeout := defaultTimeout
	if n, err := strconv.Atoi(r.URL.Query().Get("timeoutSec")); err == nil && n > 0 {
		timeout = time.Duration(n) * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	start := time.Now()
	res, err := a.Call(ctx, method, params)
	s.Log.Debug("rpc", "agent", a.ID(), "method", method, "took", time.Since(start).Round(time.Millisecond), "err", err)
	return res, err
}

type paramsFn func(r *http.Request) (json.RawMessage, error)

// queryParams builds params from query string values plus the named path
// values. Numeric query values are passed as numbers.
func queryParams(names ...string) paramsFn {
	return func(r *http.Request) (json.RawMessage, error) {
		m := map[string]any{}
		for k, v := range r.URL.Query() {
			if k == "token" || k == "timeoutSec" || len(v) == 0 {
				continue
			}
			if n, err := strconv.ParseInt(v[0], 10, 64); err == nil {
				m[k] = n
			} else {
				m[k] = v[0]
			}
		}
		for _, n := range names {
			m[n] = r.PathValue(n)
		}
		return json.Marshal(m)
	}
}

// bodyParams uses the JSON object body as params, adding the named path values.
func bodyParams(names ...string) paramsFn {
	return func(r *http.Request) (json.RawMessage, error) {
		m := map[string]any{}
		b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			return nil, err
		}
		if len(strings.TrimSpace(string(b))) > 0 {
			if err := json.Unmarshal(b, &m); err != nil {
				return nil, protocol.Errorf(protocol.CodeBadRequest, "body must be a JSON object: %v", err)
			}
		}
		for _, n := range names {
			m[n] = r.PathValue(n)
		}
		return json.Marshal(m)
	}
}

func (s *Server) call(method string, params paramsFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if params != nil {
			var err error
			if raw, err = params(r); err != nil {
				writeErr(w, err)
				return
			}
		}
		res, err := s.invoke(r, method, raw)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeRaw(w, http.StatusOK, res)
	}
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	out := []hub.Summary{}
	for _, a := range s.Hub.Agents() {
		out = append(out, a.Summary())
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	a, err := s.agent(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.Summary())
}

// handleDevices lists the devices of every connected agent, each tagged with
// agentId and agentName.
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, a := range s.Hub.Agents() {
		var devs []map[string]any
		_ = json.Unmarshal(a.Devices(), &devs)
		for _, d := range devs {
			d["agentId"] = a.ID()
			d["agentName"] = a.Name()
			out = append(out, d)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRPC calls any agent method: {"method": "...", "params": {...}, "timeoutSec": 60}.
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method     string          `json:"method"`
		Params     json.RawMessage `json:"params"`
		TimeoutSec int             `json:"timeoutSec"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&req); err != nil {
		writeErr(w, protocol.Errorf(protocol.CodeBadRequest, "invalid JSON: %v", err))
		return
	}
	if req.Method == "" {
		writeErr(w, protocol.Errorf(protocol.CodeBadRequest, "method is required"))
		return
	}
	if req.TimeoutSec > 0 {
		q := r.URL.Query()
		q.Set("timeoutSec", strconv.Itoa(req.TimeoutSec))
		r.URL.RawQuery = q.Encode()
	}
	res, err := s.invoke(r, req.Method, req.Params)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}

func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	res, err := s.invoke(r, "device.screenshot", mustJSON(map[string]string{"serial": r.PathValue("serial")}))
	if err != nil {
		writeErr(w, err)
		return
	}
	var shot struct {
		Mime string `json:"mime"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(res, &shot); err != nil {
		writeErr(w, err)
		return
	}
	img, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		writeErr(w, fmt.Errorf("decode screenshot: %w", err))
		return
	}
	if shot.Mime == "" {
		shot.Mime = "image/png"
	}
	w.Header().Set("Content-Type", shot.Mime)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(img)
}

func (s *Server) handleSource(w http.ResponseWriter, r *http.Request) {
	res, err := s.invoke(r, "device.source", mustJSON(map[string]string{"serial": r.PathValue("serial")}))
	if err != nil {
		writeErr(w, err)
		return
	}
	var src struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(res, &src); err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Write([]byte(src.Source))
}

// handleArtifact downloads one job artifact as a file.
func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	res, err := s.invoke(r, "job.artifact", mustJSON(map[string]string{"id": r.PathValue("id"), "name": name}))
	if err != nil {
		writeErr(w, err)
		return
	}
	var art struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(res, &art); err != nil {
		writeErr(w, err)
		return
	}
	b, err := base64.StdEncoding.DecodeString(art.Data)
	if err != nil {
		writeErr(w, fmt.Errorf("decode artifact: %w", err))
		return
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(name)}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(b)
}

// handleEvents streams hub events as Server-Sent Events. ?agent= limits the
// stream to one agent; ?event= to a comma-separated list of event names.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	agentFilter := r.URL.Query().Get("agent")
	var nameFilter []string
	if v := r.URL.Query().Get("event"); v != "" {
		nameFilter = strings.Split(v, ",")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: don't buffer the stream
	evs, unsub := s.Hub.Subscribe(256)
	defer unsub()
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	keep := time.NewTicker(15 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keep.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			fl.Flush()
		case ev, ok := <-evs:
			if !ok {
				return
			}
			e := ev.Data.(hub.Event)
			if agentFilter != "" && e.AgentID != agentFilter {
				continue
			}
			if nameFilter != nil && !slices.Contains(nameFilter, e.Event) {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Event, mustJSON(e))
			fl.Flush()
		}
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	writeRaw(w, status, mustJSON(v))
}

func writeRaw(w http.ResponseWriter, status int, b []byte) {
	if len(b) == 0 {
		b = []byte("null")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
	w.Write([]byte("\n"))
}

func writeErr(w http.ResponseWriter, err error) {
	e := protocol.AsError(err)
	writeJSON(w, protocol.HTTPStatus(e.Code), map[string]any{"error": e})
}

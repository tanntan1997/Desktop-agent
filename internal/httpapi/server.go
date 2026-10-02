// Package httpapi serves the agent's local HTTP API: convenience REST routes,
// a generic POST /api/rpc that reaches every tunnel method, an SSE event
// stream, and a reverse proxy to Appium.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ababank/mobile-device-agent/internal/events"
	"github.com/ababank/mobile-device-agent/internal/protocol"
	"github.com/ababank/mobile-device-agent/internal/rpc"
)

type Server struct {
	Token          string
	AllowedOrigins []string
	Router         *rpc.Router
	Bus            *events.Bus
	Appium         http.Handler // mounted at /appium/
	Screenshot     func(ctx context.Context, serial string) ([]byte, error)
	Source         func(ctx context.Context, serial string) (string, error)
	Log            *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	api := http.NewServeMux()
	api.HandleFunc("POST /api/rpc", s.handleRPC)
	api.HandleFunc("GET /api/events", s.handleEvents)
	api.HandleFunc("GET /api/agent", s.call("agent.info", nil))
	api.HandleFunc("GET /api/devices", s.call("devices.list", nil))
	api.HandleFunc("GET /api/devices/{serial}", s.call("device.info", pathParams("serial")))
	api.HandleFunc("GET /api/devices/{serial}/screenshot", s.handleScreenshot)
	api.HandleFunc("GET /api/devices/{serial}/source", s.handleSource)
	api.HandleFunc("POST /api/devices/{serial}/tap", s.call("device.tap", bodyWithPath("serial")))
	api.HandleFunc("POST /api/devices/{serial}/swipe", s.call("device.swipe", bodyWithPath("serial")))
	api.HandleFunc("POST /api/devices/{serial}/text", s.call("device.text", bodyWithPath("serial")))
	api.HandleFunc("POST /api/devices/{serial}/key", s.call("device.key", bodyWithPath("serial")))
	api.HandleFunc("POST /api/devices/{serial}/apps/install", s.call("app.install", bodyWithPath("serial")))
	api.HandleFunc("POST /api/devices/{serial}/apps/launch", s.call("app.launch", bodyWithPath("serial")))
	api.HandleFunc("POST /api/devices/{serial}/apps/terminate", s.call("app.terminate", bodyWithPath("serial")))
	api.HandleFunc("GET /api/jobs", s.call("job.list", nil))
	api.HandleFunc("POST /api/jobs", s.call("job.start", bodyWithPath()))
	api.HandleFunc("GET /api/jobs/{id}", s.call("job.get", pathParams("id")))
	api.HandleFunc("POST /api/jobs/{id}/cancel", s.call("job.cancel", pathParams("id")))
	api.HandleFunc("GET /api/jobs/{id}/logs", s.call("job.logs", pathParams("id")))
	if s.Appium != nil {
		api.Handle("/appium/", s.Appium)
	}
	mux.Handle("/api/", s.auth(api))
	mux.Handle("/appium/", s.auth(api))
	return s.cors(mux)
}

// auth accepts "Authorization: Bearer <token>", "?token=" (for <img src> and
// EventSource, which cannot set headers), or HTTP Basic with the token as the
// password (for Appium clients: http://agent:<token>@127.0.0.1:7100/appium).
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, pw, ok := r.BasicAuth(); ok {
			tok = pw
			r.Header.Del("Authorization") // don't leak the token to Appium
		}
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) != 1 {
			writeErr(w, protocol.Errorf(protocol.CodeForbidden, "missing or invalid API token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (slices.Contains(s.AllowedOrigins, origin) || slices.Contains(s.AllowedOrigins, "*")) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			// Chrome Private Network Access: public site -> localhost.
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type paramsFn func(r *http.Request) (json.RawMessage, error)

func pathParams(names ...string) paramsFn {
	return func(r *http.Request) (json.RawMessage, error) {
		m := map[string]any{}
		for k, v := range r.URL.Query() {
			if k != "token" && len(v) > 0 {
				m[k] = v[0]
			}
		}
		for _, n := range names {
			m[n] = r.PathValue(n)
		}
		// Numeric query params (job.logs?since=10&limit=500) are passed as numbers.
		for _, k := range []string{"since", "limit"} {
			if str, ok := m[k].(string); ok {
				if n, err := strconv.ParseInt(str, 10, 64); err == nil {
					m[k] = n
				}
			}
		}
		return json.Marshal(m)
	}
}

func bodyWithPath(names ...string) paramsFn {
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
		res, err := s.Router.Call(r.Context(), method, raw)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&req); err != nil {
		writeErr(w, protocol.Errorf(protocol.CodeBadRequest, "invalid JSON: %v", err))
		return
	}
	res, err := s.Router.Call(r.Context(), req.Method, req.Params)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}

func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	img, err := s.Screenshot(r.Context(), r.PathValue("serial"))
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(img)
}

func (s *Server) handleSource(w http.ResponseWriter, r *http.Request) {
	xml, err := s.Source(r.Context(), r.PathValue("serial"))
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Write([]byte(xml))
}

// handleEvents streams bus events as Server-Sent Events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	evs, unsub := s.Bus.Subscribe(256)
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
			b, _ := json.Marshal(ev.Data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, b)
			fl.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	e := protocol.AsError(err)
	writeJSON(w, protocol.HTTPStatus(e.Code), map[string]any{"error": e})
}

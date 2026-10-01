// Command hub-example is a minimal reference implementation of the web-server
// side of the tunnel. It accepts agent connections on /agent and exposes a
// small REST API to call any agent method. Use it to test agents and as a
// template for the real backend.
//
//	go run ./cmd/hub-example -listen :8080 -token secret
//	curl localhost:8080/agents
//	curl -XPOST localhost:8080/agents/<id>/rpc -d '{"method":"devices.list"}'
//	curl -o s.png localhost:8080/agents/<id>/devices/<serial>/screenshot
package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ababank/mobile-device-agent/internal/protocol"
)

type agentConn struct {
	id      string
	conn    *websocket.Conn
	writeMu sync.Mutex

	mu      sync.Mutex
	hello   protocol.Hello
	devices json.RawMessage
	pending map[string]chan protocol.Message
	seq     atomic.Int64
}

func (a *agentConn) write(m protocol.Message) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_ = a.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return a.conn.WriteJSON(m)
}

// Call sends a request and waits for the matching response.
func (a *agentConn) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *protocol.Error) {
	id := strconv.FormatInt(a.seq.Add(1), 10)
	ch := make(chan protocol.Message, 1)
	a.mu.Lock()
	a.pending[id] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()
	if err := a.write(protocol.Message{Type: protocol.TypeRequest, ID: id, Method: method, Params: params}); err != nil {
		return nil, &protocol.Error{Code: protocol.CodeUnavailable, Message: err.Error()}
	}
	select {
	case m := <-ch:
		return m.Result, m.Error
	case <-ctx.Done():
		_ = a.write(protocol.Message{Type: protocol.TypeCancel, ID: id})
		return nil, &protocol.Error{Code: protocol.CodeTimeout, Message: ctx.Err().Error()}
	}
}

type hub struct {
	token string
	log   *slog.Logger
	mu    sync.RWMutex
	conns map[string]*agentConn
}

func (h *hub) get(id string) *agentConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.conns[id]
}

var upgrader = websocket.Upgrader{EnableCompression: true}

func (h *hub) handleAgent(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if h.token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(h.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(128 << 20)
	a := &agentConn{id: r.Header.Get("X-Agent-Id"), conn: conn, pending: map[string]chan protocol.Message{}}
	defer conn.Close()

	for {
		var m protocol.Message
		if err := conn.ReadJSON(&m); err != nil {
			h.log.Info("agent disconnected", "agent", a.id, "err", err)
			break
		}
		switch m.Type {
		case protocol.TypeHello:
			var hello protocol.Hello
			_ = json.Unmarshal(m.Data, &hello)
			a.mu.Lock()
			a.hello = hello
			a.devices, _ = json.Marshal(hello.Devices)
			a.mu.Unlock()
			if a.id == "" {
				a.id = hello.AgentID
			}
			h.mu.Lock()
			if old := h.conns[a.id]; old != nil {
				old.conn.Close()
			}
			h.conns[a.id] = a
			h.mu.Unlock()
			h.log.Info("agent connected", "agent", a.id, "name", hello.Name, "os", hello.OS, "version", hello.Version)
		case protocol.TypeResponse:
			a.mu.Lock()
			ch := a.pending[m.ID]
			a.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case protocol.TypeEvent:
			if m.Event == "devices.changed" {
				a.mu.Lock()
				a.devices = m.Data
				a.mu.Unlock()
			}
			if m.Event != "job.log" {
				h.log.Info("event", "agent", a.id, "event", m.Event, "data", truncate(string(m.Data), 300))
			} else {
				h.log.Debug("event", "agent", a.id, "event", m.Event, "data", string(m.Data))
			}
		}
	}
	h.mu.Lock()
	if h.conns[a.id] == a {
		delete(h.conns, a.id)
	}
	h.mu.Unlock()
}

func (h *hub) handleList(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	out := []map[string]any{}
	for _, a := range h.conns {
		a.mu.Lock()
		out = append(out, map[string]any{"id": a.id, "hello": a.hello, "devices": a.devices})
		a.mu.Unlock()
	}
	h.mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

func (h *hub) handleRPC(w http.ResponseWriter, r *http.Request) {
	a := h.get(r.PathValue("id"))
	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not connected"})
		return
	}
	var req struct {
		Method     string          `json:"method"`
		Params     json.RawMessage `json:"params"`
		TimeoutSec int             `json:"timeoutSec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.TimeoutSec <= 0 {
		req.TimeoutSec = 120
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(req.TimeoutSec)*time.Second)
	defer cancel()
	res, perr := a.Call(ctx, req.Method, req.Params)
	if perr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": perr})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}

func (h *hub) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	a := h.get(r.PathValue("id"))
	if a == nil {
		http.Error(w, "agent not connected", http.StatusNotFound)
		return
	}
	params, _ := json.Marshal(map[string]string{"serial": r.PathValue("serial")})
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, perr := a.Call(ctx, "device.screenshot", params)
	if perr != nil {
		http.Error(w, perr.Code+": "+perr.Message, http.StatusBadGateway)
		return
	}
	var shot struct{ Data string }
	_ = json.Unmarshal(res, &shot)
	img, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(img)
}

func main() {
	listen := flag.String("listen", ":8080", "listen address")
	token := flag.String("token", "", "token agents must present (empty = no auth)")
	flag.Parse()

	h := &hub{token: *token, conns: map[string]*agentConn{},
		log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent", h.handleAgent)
	mux.HandleFunc("GET /agents", h.handleList)
	mux.HandleFunc("POST /agents/{id}/rpc", h.handleRPC)
	mux.HandleFunc("GET /agents/{id}/devices/{serial}/screenshot", h.handleScreenshot)
	h.log.Info("hub listening", "addr", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

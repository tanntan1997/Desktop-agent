// Package hub is the web-server side of the agent tunnel. It accepts agent
// WebSocket connections, keeps a registry of connected agents and their
// devices, routes requests to an agent and matches the responses, and fans
// agent events out to subscribers.
package hub

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ababank/mobile-device-agent/internal/events"
	"github.com/ababank/mobile-device-agent/internal/protocol"
)

const (
	helloTimeout = 15 * time.Second
	readTimeout  = 60 * time.Second // agent pings every 20s
	writeTimeout = 30 * time.Second
	maxFrame     = 128 << 20
)

// Hub-generated events, published alongside the events agents send.
const (
	EventAgentConnected    = "agent.connected"
	EventAgentDisconnected = "agent.disconnected"
)

// Event is an agent event tagged with the agent that produced it.
type Event struct {
	AgentID string          `json:"agentId"`
	Event   string          `json:"event"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Hub tracks connected agents. The zero value is not usable; use New.
type Hub struct {
	// SharedToken, if set, is accepted from any agent.
	SharedToken string
	// AgentTokens maps agent ID -> token for per-agent credentials.
	AgentTokens map[string]string
	Log         *slog.Logger

	bus      *events.Bus
	upgrader websocket.Upgrader

	mu     sync.RWMutex
	agents map[string]*Agent
}

func New(log *slog.Logger) *Hub {
	return &Hub{
		Log:      log,
		bus:      events.NewBus(),
		upgrader: websocket.Upgrader{EnableCompression: true},
		agents:   map[string]*Agent{},
	}
}

// Subscribe returns a stream of Events and a function that unsubscribes.
func (h *Hub) Subscribe(buffer int) (<-chan events.Event, func()) { return h.bus.Subscribe(buffer) }

// Get returns a connected agent or nil.
func (h *Hub) Get(id string) *Agent {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.agents[id]
}

// Agents returns all connected agents sorted by name.
func (h *Hub) Agents() []*Agent {
	h.mu.RLock()
	out := make([]*Agent, 0, len(h.agents))
	for _, a := range h.agents {
		out = append(out, a)
	}
	h.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func (h *Hub) authorized(agentID, token string) bool {
	if h.SharedToken == "" && len(h.AgentTokens) == 0 {
		return true
	}
	if h.SharedToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(h.SharedToken)) == 1 {
		return true
	}
	want, ok := h.AgentTokens[agentID]
	return ok && subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
}

// ServeHTTP upgrades an agent connection and serves it until it closes.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("X-Agent-Id")
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if id == "" {
		http.Error(w, "missing X-Agent-Id", http.StatusBadRequest)
		return
	}
	if !h.authorized(id, tok) {
		h.Log.Warn("agent rejected", "agent", id, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxFrame)

	a := &Agent{id: id, remote: r.RemoteAddr, conn: conn, pending: map[string]chan protocol.Message{}, done: make(chan struct{})}
	if err := a.readHello(); err != nil {
		h.Log.Warn("agent handshake failed", "agent", id, "err", err)
		return
	}
	h.register(a)
	defer h.unregister(a)

	err = a.readLoop(h.publish)
	h.Log.Info("agent disconnected", "agent", id, "name", a.Name(), "err", err)
}

func (h *Hub) register(a *Agent) {
	h.mu.Lock()
	old := h.agents[a.id]
	h.agents[a.id] = a
	h.mu.Unlock()
	if old != nil {
		// Same agent reconnected before we noticed the old socket died.
		old.close(errors.New("replaced by a newer connection"))
	}
	hello := a.Hello()
	h.Log.Info("agent connected", "agent", a.id, "name", hello.Name, "os", hello.OS,
		"version", hello.Version, "remote", a.remote)
	h.publish(a.id, EventAgentConnected, a.Summary())
}

func (h *Hub) unregister(a *Agent) {
	a.close(errors.New("agent disconnected"))
	h.mu.Lock()
	current := h.agents[a.id] == a
	if current {
		delete(h.agents, a.id)
	}
	h.mu.Unlock()
	if current {
		h.publish(a.id, EventAgentDisconnected, map[string]string{"id": a.id})
	}
}

// Publish emits an event to subscribers as if agentID had sent it. Server
// features use it to stream their own progress next to agent events.
func (h *Hub) Publish(agentID, name string, data any) { h.publish(agentID, name, data) }

func (h *Hub) publish(agentID, name string, data any) {
	raw, ok := data.(json.RawMessage)
	if !ok {
		raw, _ = json.Marshal(data)
	}
	h.bus.Publish(name, Event{AgentID: agentID, Event: name, Data: raw})
}

// Agent is one connected agent.
type Agent struct {
	id          string
	remote      string
	conn        *websocket.Conn
	writeMu     sync.Mutex
	seq         atomic.Int64
	connectedAt time.Time

	mu      sync.Mutex
	hello   protocol.Hello
	devices json.RawMessage
	pending map[string]chan protocol.Message
	closed  error
	done    chan struct{}
}

// Summary is the JSON view of an agent used by the REST API.
type Summary struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	OS          string          `json:"os"`
	Arch        string          `json:"arch"`
	Hostname    string          `json:"hostname"`
	Remote      string          `json:"remote"`
	ConnectedAt time.Time       `json:"connectedAt"`
	Methods     []string        `json:"methods"`
	Runners     []string        `json:"runners"`
	Devices     json.RawMessage `json:"devices"`
}

func (a *Agent) ID() string { return a.id }

func (a *Agent) Name() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hello.Name != "" {
		return a.hello.Name
	}
	return a.id
}

func (a *Agent) Hello() protocol.Hello {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hello
}

// Devices returns the agent's last reported device list (JSON array).
func (a *Agent) Devices() json.RawMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.devices
}

func (a *Agent) Summary() Summary {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Summary{
		ID: a.id, Name: a.hello.Name, Version: a.hello.Version, OS: a.hello.OS, Arch: a.hello.Arch,
		Hostname: a.hello.Hostname, Remote: a.remote, ConnectedAt: a.connectedAt,
		Methods: a.hello.Methods, Runners: a.hello.Runners, Devices: a.devices,
	}
}

func (a *Agent) write(m protocol.Message) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_ = a.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return a.conn.WriteJSON(m)
}

// Call sends a request to the agent and waits for its response. If ctx ends
// first, a cancel frame is sent so the agent aborts the work.
func (a *Agent) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, ok := params.(json.RawMessage)
	if !ok && params != nil {
		var err error
		if raw, err = json.Marshal(params); err != nil {
			return nil, protocol.Errorf(protocol.CodeBadRequest, "params: %v", err)
		}
	}
	id := strconv.FormatInt(a.seq.Add(1), 10)
	ch := make(chan protocol.Message, 1)
	a.mu.Lock()
	if a.closed != nil {
		a.mu.Unlock()
		return nil, protocol.Errorf(protocol.CodeUnavailable, "agent %s: %v", a.id, a.closed)
	}
	a.pending[id] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()

	if err := a.write(protocol.Message{Type: protocol.TypeRequest, ID: id, Method: method, Params: raw}); err != nil {
		return nil, protocol.Errorf(protocol.CodeUnavailable, "agent %s: %v", a.id, err)
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-a.done:
		return nil, protocol.Errorf(protocol.CodeUnavailable, "agent %s disconnected during %s", a.id, method)
	case <-ctx.Done():
		_ = a.write(protocol.Message{Type: protocol.TypeCancel, ID: id})
		return nil, protocol.AsError(ctx.Err())
	}
}

func (a *Agent) readHello() error {
	_ = a.conn.SetReadDeadline(time.Now().Add(helloTimeout))
	var m protocol.Message
	if err := a.conn.ReadJSON(&m); err != nil {
		return err
	}
	if m.Type != protocol.TypeHello {
		return errors.New("first frame is " + strconv.Quote(m.Type) + ", want hello")
	}
	var hello protocol.Hello
	if err := json.Unmarshal(m.Data, &hello); err != nil {
		return err
	}
	if hello.Protocol != protocol.Version {
		return errors.New("unsupported protocol version " + strconv.Itoa(hello.Protocol))
	}
	devices, _ := json.Marshal(hello.Devices)
	if string(devices) == "null" {
		devices = json.RawMessage("[]")
	}
	a.mu.Lock()
	a.hello = hello
	a.devices = devices
	a.connectedAt = time.Now().UTC()
	a.mu.Unlock()
	return nil
}

func (a *Agent) readLoop(publish func(agentID, name string, data any)) error {
	extend := func() { _ = a.conn.SetReadDeadline(time.Now().Add(readTimeout)) }
	extend()
	a.conn.SetPingHandler(func(data string) error {
		extend()
		a.writeMu.Lock()
		defer a.writeMu.Unlock()
		err := a.conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(writeTimeout))
		if errors.Is(err, websocket.ErrCloseSent) {
			return nil
		}
		return err
	})
	for {
		var m protocol.Message
		if err := a.conn.ReadJSON(&m); err != nil {
			return err
		}
		extend()
		switch m.Type {
		case protocol.TypeResponse:
			a.mu.Lock()
			ch := a.pending[m.ID]
			a.mu.Unlock()
			if ch != nil {
				ch <- m // buffered, and each ID is answered once
			}
		case protocol.TypeEvent:
			if m.Event == "devices.changed" {
				a.mu.Lock()
				a.devices = m.Data
				a.mu.Unlock()
			}
			publish(a.id, m.Event, m.Data)
		}
	}
}

// close fails pending and future calls and closes the socket. Safe to call
// more than once.
func (a *Agent) close(reason error) {
	a.mu.Lock()
	if a.closed != nil {
		a.mu.Unlock()
		return
	}
	a.closed = reason
	close(a.done)
	a.mu.Unlock()
	a.writeMu.Lock()
	_ = a.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason.Error()), time.Now().Add(time.Second))
	a.writeMu.Unlock()
	a.conn.Close()
}

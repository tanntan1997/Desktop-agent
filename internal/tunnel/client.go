// Package tunnel keeps an outbound WebSocket to the web server. The agent
// dials out, so it works behind NAT and corporate firewalls without opening
// inbound ports; the server sends requests down the socket and receives
// responses and events back.
package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ababank/mobile-device-agent/internal/events"
	"github.com/ababank/mobile-device-agent/internal/protocol"
	"github.com/ababank/mobile-device-agent/internal/rpc"
)

const (
	pingEvery    = 20 * time.Second
	readTimeout  = 60 * time.Second
	writeTimeout = 30 * time.Second
	maxFrame     = 128 << 20
)

type Client struct {
	URL         string
	Token       string
	AgentID     string
	Router      *rpc.Router
	Bus         *events.Bus
	Hello       func() protocol.Hello
	MaxInFlight int
	Log         *slog.Logger

	mu        sync.RWMutex
	connected bool
}

func (c *Client) Connected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// Run reconnects with jittered exponential backoff until ctx is cancelled.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for {
		start := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		wait := backoff/2 + rand.N(backoff/2+1)
		c.Log.Warn("tunnel disconnected", "url", c.URL, "err", err, "retryIn", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *Client) setConnected(v bool) {
	c.mu.Lock()
	c.connected = v
	c.mu.Unlock()
}

func (c *Client) session(ctx context.Context) error {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+c.Token)
	hdr.Set("X-Agent-Id", c.AgentID)
	dialer := websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  15 * time.Second,
		EnableCompression: true,
	}
	conn, resp, err := dialer.DialContext(ctx, c.URL, hdr)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial: %w (HTTP %s)", err, resp.Status)
		}
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	conn.SetReadLimit(maxFrame)

	c.setConnected(true)
	defer c.setConnected(false)
	c.Log.Info("tunnel connected", "url", c.URL)

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	out := make(chan protocol.Message, 256)
	send := func(m protocol.Message) bool {
		select {
		case out <- m:
			return true
		case <-sctx.Done():
			return false
		}
	}

	// Writer: the only goroutine that writes data frames.
	writeErr := make(chan error, 1)
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "agent shutting down"),
					time.Now().Add(time.Second))
				writeErr <- nil
				return
			case m := <-out:
				_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
				if err := conn.WriteJSON(m); err != nil {
					writeErr <- err
					cancel()
					return
				}
			case <-t.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
					writeErr <- err
					cancel()
					return
				}
			}
		}
	}()

	hello, _ := json.Marshal(c.Hello())
	send(protocol.Message{Type: protocol.TypeHello, Data: hello})

	// Event forwarder.
	evs, unsub := c.Bus.Subscribe(1024)
	defer unsub()
	go func() {
		for {
			select {
			case <-sctx.Done():
				return
			case ev, ok := <-evs:
				if !ok {
					return
				}
				data, err := json.Marshal(ev.Data)
				if err != nil {
					continue
				}
				send(protocol.Message{Type: protocol.TypeEvent, Event: ev.Name, Data: data})
			}
		}
	}()

	// Reader.
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(readTimeout)) })
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(writeTimeout))
	})

	sem := make(chan struct{}, max(c.MaxInFlight, 1))
	var inflight sync.Map // request id -> context.CancelFunc
	var wg sync.WaitGroup
	defer wg.Wait()

	readErr := make(chan error, 1)
	go func() {
		for {
			var m protocol.Message
			if err := conn.ReadJSON(&m); err != nil {
				readErr <- err
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
			switch m.Type {
			case protocol.TypeRequest:
				rctx, rcancel := context.WithCancel(sctx)
				inflight.Store(m.ID, rcancel)
				wg.Add(1)
				go func(m protocol.Message) {
					defer wg.Done()
					defer inflight.Delete(m.ID)
					defer rcancel()
					select {
					case sem <- struct{}{}:
						defer func() { <-sem }()
					case <-rctx.Done():
						return
					}
					send(c.handle(rctx, m))
				}(m)
			case protocol.TypeCancel:
				if f, ok := inflight.Load(m.ID); ok {
					f.(context.CancelFunc)()
				}
			default:
				c.Log.Debug("ignoring tunnel frame", "type", m.Type)
			}
		}
	}()

	select {
	case <-sctx.Done():
		if err := <-writeErr; err != nil {
			return err
		}
		return ctx.Err()
	case err := <-readErr:
		cancel()
		return err
	}
}

func (c *Client) handle(ctx context.Context, m protocol.Message) protocol.Message {
	start := time.Now()
	resp := protocol.Message{Type: protocol.TypeResponse, ID: m.ID}
	res, err := c.Router.Call(ctx, m.Method, m.Params)
	if err == nil {
		resp.Result, err = json.Marshal(res)
	}
	if err != nil {
		resp.Error = protocol.AsError(err)
		resp.Result = nil
	}
	lvl := slog.LevelDebug
	if err != nil && !errors.Is(err, context.Canceled) {
		lvl = slog.LevelWarn
	}
	c.Log.Log(ctx, lvl, "rpc", "method", m.Method, "id", m.ID, "took", time.Since(start).Round(time.Millisecond), "err", err)
	return resp
}

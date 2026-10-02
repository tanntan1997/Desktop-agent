package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ababank/mobile-device-agent/internal/autotest"
	"github.com/ababank/mobile-device-agent/internal/events"
	"github.com/ababank/mobile-device-agent/internal/hub"
	"github.com/ababank/mobile-device-agent/internal/protocol"
	"github.com/ababank/mobile-device-agent/internal/rpc"
	"github.com/ababank/mobile-device-agent/internal/tunnel"
)

const (
	agentToken = "agent-secret"
	apiToken   = "api-secret"
)

type fixture struct {
	url       string
	bus       *events.Bus // the fake agent's bus
	stopAgent func()
	stop      func()
}

// start runs a hub server and connects a real tunnel.Client backed by a fake
// method table.
func start(t *testing.T) *fixture {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := hub.New(log)
	h.SharedToken = agentToken
	srv := httptest.NewServer((&Server{Hub: h, APITokens: []string{apiToken}, Log: log}).Handler())

	png := []byte("\x89PNG fake")
	r := rpc.NewRouter()
	r.Handle("device.screenshot", rpc.Typed(func(ctx context.Context, p struct{ Serial string }) (any, error) {
		return map[string]any{"mime": "image/png", "data": base64.StdEncoding.EncodeToString(png)}, nil
	}))
	r.Handle("device.tap", rpc.Typed(func(ctx context.Context, p struct {
		Serial string
		X, Y   int
	}) (any, error) {
		if p.Serial != "emu-1" || p.X != 10 || p.Y != 20 {
			return nil, protocol.Errorf(protocol.CodeBadRequest, "got %+v", p)
		}
		return map[string]bool{"ok": true}, nil
	}))
	r.Handle("slow", func(ctx context.Context, _ json.RawMessage) (any, error) {
		<-ctx.Done() // only returns once the hub sends a cancel
		return nil, ctx.Err()
	})

	bus := events.NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	c := &tunnel.Client{
		URL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent", Token: agentToken, AgentID: "a1",
		Router: r, Bus: bus, MaxInFlight: 4, Log: log,
		Hello: func() protocol.Hello {
			return protocol.Hello{Protocol: protocol.Version, AgentID: "a1", Name: "qa-mac",
				Devices: []map[string]string{{"serial": "emu-1", "platform": "android", "state": "online"}}}
		},
	}
	go c.Run(ctx)
	waitFor(t, func() bool { return h.Get("a1") != nil })
	return &fixture{url: srv.URL, bus: bus, stopAgent: cancel, stop: func() { cancel(); srv.Close() }}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met in time")
}

func do(t *testing.T, method, url, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func TestAPI(t *testing.T) {
	f := start(t)
	defer f.stop()

	t.Run("auth required", func(t *testing.T) {
		res, err := http.Get(f.url + "/api/agents")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status %d", res.StatusCode)
		}
	})

	t.Run("devices", func(t *testing.T) {
		code, b := do(t, "GET", f.url+"/api/devices", "")
		if code != 200 || !strings.Contains(string(b), `"agentId":"a1"`) || !strings.Contains(string(b), `"serial":"emu-1"`) {
			t.Fatalf("%d %s", code, b)
		}
	})

	t.Run("tap", func(t *testing.T) {
		code, b := do(t, "POST", f.url+"/api/agents/a1/devices/emu-1/tap", `{"x":10,"y":20}`)
		if code != 200 || strings.TrimSpace(string(b)) != `{"ok":true}` {
			t.Fatalf("%d %s", code, b)
		}
	})

	t.Run("screenshot", func(t *testing.T) {
		code, b := do(t, "GET", f.url+"/api/agents/a1/devices/emu-1/screenshot", "")
		if code != 200 || string(b) != "\x89PNG fake" {
			t.Fatalf("%d %q", code, b)
		}
	})

	t.Run("unknown method", func(t *testing.T) {
		code, b := do(t, "POST", f.url+"/api/agents/a1/rpc", `{"method":"nope"}`)
		if code != 404 || !strings.Contains(string(b), "not_found") {
			t.Fatalf("%d %s", code, b)
		}
	})

	t.Run("unknown agent", func(t *testing.T) {
		code, _ := do(t, "GET", f.url+"/api/agents/zzz", "")
		if code != 404 {
			t.Fatalf("status %d", code)
		}
	})

	t.Run("timeout cancels", func(t *testing.T) {
		code, b := do(t, "POST", f.url+"/api/agents/a1/rpc", `{"method":"slow","timeoutSec":1}`)
		if code != http.StatusGatewayTimeout {
			t.Fatalf("%d %s", code, b)
		}
	})
}

func TestEventsAndDevicesChanged(t *testing.T) {
	f := start(t)
	defer f.stop()

	req, _ := http.NewRequest("GET", f.url+"/api/events?token="+apiToken+"&event=devices.changed", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	buf := make([]byte, 64)
	if _, err := res.Body.Read(buf); err != nil { // ": connected"
		t.Fatal(err)
	}

	f.bus.Publish("devices.changed", []map[string]string{{"serial": "emu-2", "platform": "android", "state": "online"}})

	got := make(chan string, 1)
	go func() {
		b := make([]byte, 4096)
		n, _ := res.Body.Read(b)
		got <- string(b[:n])
	}()
	select {
	case s := <-got:
		if !strings.Contains(s, "event: devices.changed") || !strings.Contains(s, `"agentId":"a1"`) {
			t.Fatalf("unexpected SSE frame %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	waitFor(t, func() bool {
		_, b := do(t, "GET", f.url+"/api/devices", "")
		return strings.Contains(string(b), "emu-2")
	})
}

func TestAgentDisconnectFailsPendingCalls(t *testing.T) {
	f := start(t)
	defer f.stop()

	type result struct {
		code int
		body []byte
	}
	done := make(chan result, 1)
	go func() {
		code, b := do(t, "POST", f.url+"/api/agents/a1/rpc", `{"method":"slow","timeoutSec":60}`)
		done <- result{code, b}
	}()
	time.Sleep(200 * time.Millisecond)
	f.stopAgent()
	select {
	case r := <-done:
		if r.code != http.StatusServiceUnavailable {
			t.Fatalf("%d %s", r.code, r.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pending call not failed on disconnect")
	}
	waitFor(t, func() bool {
		code, _ := do(t, "GET", f.url+"/api/agents/a1", "")
		return code == http.StatusNotFound
	})
}

func TestAgentRejectedWithBadToken(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := hub.New(log)
	h.SharedToken = agentToken
	srv := httptest.NewServer((&Server{Hub: h, Log: log}).Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/agent", nil)
	req.Header.Set("X-Agent-Id", "a1")
	req.Header.Set("Authorization", "Bearer wrong")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", res.StatusCode)
	}
}

func TestAutoTestAPI(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := hub.New(log)
	store, err := autotest.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := autotest.NewService(h, store, nil, autotest.ExploreConfig{}, log)
	srv := httptest.NewServer((&Server{Hub: h, APITokens: []string{apiToken}, AutoTest: svc, Log: log}).Handler())
	defer srv.Close()

	code, b := do(t, "POST", srv.URL+"/api/tests", `{"prompt":"Verify Exchange menu\n1. open MBB","variables":{"PIN":"1234"}}`)
	if code != http.StatusCreated || strings.Contains(string(b), "1234") {
		t.Fatalf("create: %d %s", code, b)
	}
	var created autotest.Test
	_ = json.Unmarshal(b, &created)
	if created.Name != "Verify Exchange menu" || created.Variables["PIN"] != "******" {
		t.Fatalf("created %+v", created)
	}

	// Sending the mask back keeps the stored value; steps can be set by hand.
	code, b = do(t, "PUT", srv.URL+"/api/tests/"+created.ID,
		`{"name":"Exchange","prompt":"p","variables":{"PIN":"******","USER":"bob"},"steps":[{"action":"wait","durationMs":10}]}`)
	if code != 200 {
		t.Fatalf("update: %d %s", code, b)
	}
	stored, _ := store.GetTest(created.ID)
	if stored.Variables["PIN"] != "1234" || stored.Variables["USER"] != "bob" || len(stored.Steps) != 1 {
		t.Fatalf("stored %+v", stored)
	}

	if code, b = do(t, "POST", srv.URL+"/api/tests/"+created.ID+"/explore", `{"agentId":"a","serial":"s"}`); code != http.StatusServiceUnavailable {
		t.Fatalf("explore without AI: %d %s", code, b)
	}
	if code, b = do(t, "POST", srv.URL+"/api/tests/"+created.ID+"/run", `{"agentId":"nope","serial":"s"}`); code != http.StatusNotFound {
		t.Fatalf("run on missing agent: %d %s", code, b)
	}
	if code, _ = do(t, "GET", srv.URL+"/api/tests/../../etc", ""); code == 200 {
		t.Fatal("path traversal accepted")
	}
	if code, _ = do(t, "GET", srv.URL+"/api/runs/0123456789abcdef/files/..%2Fx.json", ""); code == 200 {
		t.Fatal("file traversal accepted")
	}
	if code, _ = do(t, "DELETE", srv.URL+"/api/tests/"+created.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
}

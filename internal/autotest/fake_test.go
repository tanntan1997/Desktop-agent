package autotest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"strings"
	"sync"
	"testing"

	"github.com/openai/openai-go/v3"
)

// fakePhone is a tiny app: launcher -> login (PIN field + Login button) ->
// dashboard (Exchange menu). Logging in needs PIN 1234.
type fakePhone struct {
	mu      sync.Mutex
	screen  string
	pin     string
	focused bool
	taps    int
	shot    []byte
}

const (
	fakeW, fakeH = 1080, 2400
	fakeApp      = "com.example.mbb"
)

func newFakePhone(t *testing.T) *fakePhone {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, fakeW, fakeH))); err != nil {
		t.Fatal(err)
	}
	return &fakePhone{screen: "launcher", shot: buf.Bytes()}
}

func node(class, id, text, desc, bounds string, clickable bool) string {
	return fmt.Sprintf(`<node class="android.widget.%s" resource-id="%s" text="%s" content-desc="%s" bounds="%s" clickable="%t" enabled="true"/>`,
		class, id, text, desc, bounds, clickable)
}

func (p *fakePhone) xml() string {
	var nodes []string
	switch p.screen {
	case "launcher":
		nodes = append(nodes, node("TextView", "", "MBB", "MBB", "[100,100][300,300]", true))
	case "login":
		nodes = append(nodes,
			node("TextView", "", "Welcome back", "", "[100,200][980,300]", false),
			node("EditText", fakeApp+":id/pin", strings.Repeat("•", len(p.pin)), "", "[100,800][980,950]", true),
			node("Button", fakeApp+":id/login", "Login", "", "[100,1000][980,1150]", true))
	case "dashboard":
		nodes = append(nodes,
			node("TextView", "", "Balance 1,234.56 USD", "", "[100,200][980,300]", false),
			node("TextView", fakeApp+":id/menu_transfer", "Transfer", "", "[100,1800][500,1900]", true),
			node("TextView", fakeApp+":id/menu_exchange", "Exchange", "", "[580,1800][980,1900]", true))
	}
	return fmt.Sprintf(`<?xml version="1.0"?><hierarchy rotation="0"><node class="android.widget.FrameLayout" resource-id="" text="" content-desc="" bounds="[0,0][%d,%d]" clickable="false" enabled="true">%s</node></hierarchy>`,
		fakeW, fakeH, strings.Join(nodes, ""))
}

func (p *fakePhone) Screenshot(ctx context.Context) ([]byte, error) { return p.shot, nil }

func (p *fakePhone) Source(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.xml(), nil
}

func (p *fakePhone) Tap(ctx context.Context, x, y int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.taps++
	in := func(x1, y1, x2, y2 int) bool { return x >= x1 && x <= x2 && y >= y1 && y <= y2 }
	switch {
	case p.screen == "launcher" && in(100, 100, 300, 300):
		p.screen = "login"
	case p.screen == "login" && in(100, 800, 980, 950):
		p.focused = true
	case p.screen == "login" && in(100, 1000, 980, 1150) && p.pin == "1234":
		p.screen = "dashboard"
	}
	return nil
}

func (p *fakePhone) Swipe(ctx context.Context, x1, y1, x2, y2, ms int) error { return nil }

func (p *fakePhone) Text(ctx context.Context, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.screen == "login" && p.focused {
		p.pin += text
	}
	return nil
}

func (p *fakePhone) Key(ctx context.Context, key string) error { return nil }

func (p *fakePhone) Launch(ctx context.Context, appID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if appID != fakeApp {
		return fmt.Errorf("app %s not installed", appID)
	}
	p.screen, p.pin, p.focused = "login", "", false
	return nil
}

func (p *fakePhone) Terminate(ctx context.Context, appID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.screen = "launcher"
	return nil
}

// scriptedLLM replays canned assistant turns. Each turn is a list of tool
// calls (name, input JSON); a turn may instead reference elements by
// resolving them lazily from the latest request (see elementOf).
type scriptedLLM struct {
	t        *testing.T
	turns    []func(req string) []call
	requests []string // marshaled params, to inspect what the model saw
}

type call struct {
	name  string
	input string
}

func (l *scriptedLLM) New(ctx context.Context, params openai.ChatCompletionNewParams) (*openai.ChatCompletion, error) {
	b, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	req := string(b)
	l.requests = append(l.requests, req)
	n := len(l.requests)
	if n > len(l.turns) {
		l.t.Fatalf("model called %d times, script has %d turns", n, len(l.turns))
	}
	var calls []map[string]any
	for i, c := range l.turns[n-1](req) {
		calls = append(calls, map[string]any{"id": fmt.Sprintf("call_%d_%d", n, i), "type": "function",
			"function": map[string]any{"name": c.name, "arguments": c.input}})
	}
	raw, _ := json.Marshal(map[string]any{
		"id": fmt.Sprintf("chatcmpl_%d", n), "object": "chat.completion", "created": 0, "model": DefaultModel,
		"choices": []map[string]any{{"index": 0, "finish_reason": "tool_calls",
			"message": map[string]any{"role": "assistant", "content": fmt.Sprintf("turn %d", n), "refusal": nil, "tool_calls": calls}}},
		"usage": map[string]any{"prompt_tokens": 1500, "completion_tokens": 100, "total_tokens": 1600,
			"prompt_tokens_details":     map[string]any{"cached_tokens": 500},
			"completion_tokens_details": map[string]any{"reasoning_tokens": 40}},
	})
	var resp openai.ChatCompletion
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// elementOf finds "#N <Class> \"<text>\"" or "id=<id>" in the last
// observation text of a request and returns N, like the model would.
func elementOf(t *testing.T, req, needle string) int {
	t.Helper()
	// Requests are JSON; observation text has escaped quotes.
	i := strings.LastIndex(req, "Elements:")
	if i < 0 {
		t.Fatalf("no observation in request")
	}
	obs := strings.ReplaceAll(req[i:], `\"`, `"`)
	for _, line := range strings.Split(obs, `\n`) {
		if strings.Contains(line, needle) && strings.HasPrefix(line, "#") {
			var n int
			fmt.Sscanf(line, "#%d", &n)
			return n
		}
	}
	t.Fatalf("element %q not in observation:\n%s", needle, obs)
	return -1
}

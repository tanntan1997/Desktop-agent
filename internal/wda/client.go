// Package wda is a minimal WebDriverAgent client covering what the agent
// needs: screenshots, page source, W3C pointer actions, typing and app control.
package wda

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Error is a WebDriver error response.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("wda: HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

type Client struct {
	BaseURL string
	http    *http.Client

	mu  sync.Mutex
	sid string
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 2 * time.Minute},
	}
}

// Raw performs an arbitrary request against WDA and returns status and body.
func (c *Client) Raw(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	return resp.StatusCode, b, err
}

type envelope struct {
	Value     json.RawMessage `json:"value"`
	SessionID string          `json:"sessionId"`
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) (envelope, error) {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return envelope{}, err
		}
	} else if method == http.MethodPost {
		body = []byte("{}")
	}
	status, b, err := c.Raw(ctx, method, path, body)
	if err != nil {
		return envelope{}, err
	}
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return env, &Error{Status: status, Code: "invalid response", Message: fmt.Sprintf("%.200s", b)}
	}
	var werr struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if len(env.Value) > 0 && env.Value[0] == '{' {
		_ = json.Unmarshal(env.Value, &werr)
	}
	if status >= 400 || werr.Error != "" {
		return env, &Error{Status: status, Code: werr.Error, Message: werr.Message}
	}
	if out != nil && len(env.Value) > 0 && string(env.Value) != "null" {
		if err := json.Unmarshal(env.Value, out); err != nil {
			return env, fmt.Errorf("wda %s: decode: %w", path, err)
		}
	}
	return env, nil
}

// Status checks that WDA is reachable and ready.
func (c *Client) Status(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := c.call(ctx, http.MethodGet, "/status", nil, nil)
	return err
}

func (c *Client) session(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sid != "" {
		return c.sid, nil
	}
	var v struct {
		SessionID string `json:"sessionId"`
	}
	env, err := c.call(ctx, http.MethodPost, "/session",
		map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{}}}, &v)
	if err != nil {
		return "", err
	}
	c.sid = v.SessionID
	if c.sid == "" {
		c.sid = env.SessionID
	}
	if c.sid == "" {
		return "", errors.New("wda: session created without id")
	}
	return c.sid, nil
}

// withSession runs fn with a live session, recreating it once if WDA was
// restarted and the cached id went stale.
func (c *Client) withSession(ctx context.Context, fn func(sid string) error) error {
	sid, err := c.session(ctx)
	if err != nil {
		return err
	}
	err = fn(sid)
	var we *Error
	if errors.As(err, &we) && (we.Code == "invalid session id" || (we.Status == 404 && strings.Contains(strings.ToLower(we.Message), "session"))) {
		c.mu.Lock()
		if c.sid == sid {
			c.sid = ""
		}
		c.mu.Unlock()
		if sid, err = c.session(ctx); err != nil {
			return err
		}
		return fn(sid)
	}
	return err
}

func (c *Client) Screenshot(ctx context.Context) ([]byte, error) {
	var b64 string
	if _, err := c.call(ctx, http.MethodGet, "/screenshot", nil, &b64); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(b64)
}

func (c *Client) Source(ctx context.Context) (string, error) {
	var xml string
	_, err := c.call(ctx, http.MethodGet, "/source?format=xml", nil, &xml)
	return xml, err
}

// WindowSize returns the screen size in points.
func (c *Client) WindowSize(ctx context.Context) (w, h float64, err error) {
	var v struct{ Width, Height float64 }
	err = c.withSession(ctx, func(sid string) error {
		_, err := c.call(ctx, http.MethodGet, "/session/"+sid+"/window/size", nil, &v)
		return err
	})
	return v.Width, v.Height, err
}

func (c *Client) actions(ctx context.Context, steps []map[string]any) error {
	payload := map[string]any{"actions": []map[string]any{{
		"type":       "pointer",
		"id":         "finger1",
		"parameters": map[string]any{"pointerType": "touch"},
		"actions":    steps,
	}}}
	return c.withSession(ctx, func(sid string) error {
		_, err := c.call(ctx, http.MethodPost, "/session/"+sid+"/actions", payload, nil)
		return err
	})
}

// Tap taps at (x, y) in points.
func (c *Client) Tap(ctx context.Context, x, y float64) error {
	return c.actions(ctx, []map[string]any{
		{"type": "pointerMove", "duration": 0, "x": x, "y": y},
		{"type": "pointerDown", "button": 0},
		{"type": "pause", "duration": 60},
		{"type": "pointerUp", "button": 0},
	})
}

// Swipe drags from (x1, y1) to (x2, y2) in points.
func (c *Client) Swipe(ctx context.Context, x1, y1, x2, y2 float64, durationMs int) error {
	return c.actions(ctx, []map[string]any{
		{"type": "pointerMove", "duration": 0, "x": x1, "y": y1},
		{"type": "pointerDown", "button": 0},
		{"type": "pause", "duration": 50},
		{"type": "pointerMove", "duration": durationMs, "x": x2, "y": y2},
		{"type": "pointerUp", "button": 0},
	})
}

// Type sends keystrokes to the focused element.
func (c *Client) Type(ctx context.Context, text string) error {
	return c.withSession(ctx, func(sid string) error {
		_, err := c.call(ctx, http.MethodPost, "/session/"+sid+"/wda/keys",
			map[string]any{"value": strings.Split(text, "")}, nil)
		return err
	})
}

func (c *Client) Home(ctx context.Context) error {
	_, err := c.call(ctx, http.MethodPost, "/wda/homescreen", nil, nil)
	return err
}

// PressButton presses a hardware button: home, volumeUp, volumeDown.
func (c *Client) PressButton(ctx context.Context, name string) error {
	return c.withSession(ctx, func(sid string) error {
		_, err := c.call(ctx, http.MethodPost, "/session/"+sid+"/wda/pressButton", map[string]any{"name": name}, nil)
		return err
	})
}

func (c *Client) Launch(ctx context.Context, bundleID string) error {
	return c.withSession(ctx, func(sid string) error {
		_, err := c.call(ctx, http.MethodPost, "/session/"+sid+"/wda/apps/launch", map[string]any{"bundleId": bundleID}, nil)
		return err
	})
}

func (c *Client) Terminate(ctx context.Context, bundleID string) error {
	return c.withSession(ctx, func(sid string) error {
		_, err := c.call(ctx, http.MethodPost, "/session/"+sid+"/wda/apps/terminate", map[string]any{"bundleId": bundleID}, nil)
		return err
	})
}

package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ababank/mobile-device-agent/internal/appium"
	"github.com/ababank/mobile-device-agent/internal/device"
	"github.com/ababank/mobile-device-agent/internal/jobs"
	"github.com/ababank/mobile-device-agent/internal/protocol"
	"github.com/ababank/mobile-device-agent/internal/rpc"
)

type serialParams struct {
	Serial string `json:"serial"`
}

type ok struct {
	OK bool `json:"ok"`
}

// withDevice resolves the serial to an online device before calling fn.
func withDevice[P interface{ serial() string }](s *Service, fn func(ctx context.Context, p P, info device.Info, d device.Driver) (any, error)) rpc.Handler {
	return rpc.Typed(func(ctx context.Context, p P) (any, error) {
		info, d, err := s.devices.Get(p.serial())
		if err != nil {
			return nil, err
		}
		return fn(ctx, p, info, d)
	})
}

func (p serialParams) serial() string { return p.Serial }

type tapParams struct {
	serialParams
	X int `json:"x"`
	Y int `json:"y"`
}

type swipeParams struct {
	serialParams
	X1         int `json:"x1"`
	Y1         int `json:"y1"`
	X2         int `json:"x2"`
	Y2         int `json:"y2"`
	DurationMs int `json:"durationMs"`
}

type textParams struct {
	serialParams
	Text string `json:"text"`
}

type keyParams struct {
	serialParams
	Key string `json:"key"`
}

type appParams struct {
	serialParams
	AppID string `json:"appId"`
}

type installParams struct {
	serialParams
	Path string `json:"path,omitempty"` // file on the agent host
	URL  string `json:"url,omitempty"`  // http(s) URL the agent downloads
}

type shellParams struct {
	serialParams
	Command string `json:"command"`
}

type rawHTTPParams struct {
	serialParams
	appium.Request
}

type jobIDParams struct {
	ID    string `json:"id"`
	Since int64  `json:"since,omitempty"`
	Limit int    `json:"limit,omitempty"`
	Name  string `json:"name,omitempty"`
}

func (s *Service) registerMethods() {
	r := s.router

	r.Handle("agent.info", rpc.Typed(func(ctx context.Context, _ struct{}) (any, error) {
		h := s.hello()
		return map[string]any{
			"hello":           h,
			"uptimeSec":       int(time.Since(s.started).Seconds()),
			"tunnelConnected": s.tunnel != nil && s.tunnel.Connected(),
			"tools": map[string]string{
				"adb":     s.android.Path(),
				"ios":     s.ios.Backend(),
				"appium":  s.appium.URL(),
				"goos":    runtime.GOOS,
				"version": s.version,
			},
			"discoveryErrors": s.devices.DiscoveryErrors(),
		}, nil
	}))

	r.Handle("devices.list", rpc.Typed(func(ctx context.Context, p struct {
		Refresh bool `json:"refresh"`
	}) (any, error) {
		if p.Refresh {
			s.devices.Refresh(ctx)
		}
		return s.devices.List(), nil
	}))

	r.Handle("device.info", withDevice(s, func(ctx context.Context, p serialParams, info device.Info, d device.Driver) (any, error) {
		return info, nil
	}))

	r.Handle("device.screenshot", withDevice(s, func(ctx context.Context, p serialParams, info device.Info, d device.Driver) (any, error) {
		img, err := d.Screenshot(ctx, p.Serial)
		if err != nil {
			return nil, err
		}
		res := map[string]any{"mime": "image/png", "data": base64.StdEncoding.EncodeToString(img)}
		if cfg, err := png.DecodeConfig(bytes.NewReader(img)); err == nil {
			res["width"], res["height"] = cfg.Width, cfg.Height
		}
		return res, nil
	}))

	r.Handle("device.source", withDevice(s, func(ctx context.Context, p serialParams, info device.Info, d device.Driver) (any, error) {
		xml, err := d.Source(ctx, p.Serial)
		if err != nil {
			return nil, err
		}
		return map[string]any{"format": "xml", "platform": info.Platform, "source": xml}, nil
	}))

	r.Handle("device.tap", withDevice(s, func(ctx context.Context, p tapParams, _ device.Info, d device.Driver) (any, error) {
		return ok{true}, d.Tap(ctx, p.Serial, p.X, p.Y)
	}))

	r.Handle("device.swipe", withDevice(s, func(ctx context.Context, p swipeParams, _ device.Info, d device.Driver) (any, error) {
		return ok{true}, d.Swipe(ctx, p.Serial, p.X1, p.Y1, p.X2, p.Y2, p.DurationMs)
	}))

	r.Handle("device.text", withDevice(s, func(ctx context.Context, p textParams, _ device.Info, d device.Driver) (any, error) {
		return ok{true}, d.InputText(ctx, p.Serial, p.Text)
	}))

	r.Handle("device.key", withDevice(s, func(ctx context.Context, p keyParams, _ device.Info, d device.Driver) (any, error) {
		return ok{true}, d.PressKey(ctx, p.Serial, p.Key)
	}))

	r.Handle("device.shell", withDevice(s, func(ctx context.Context, p shellParams, _ device.Info, d device.Driver) (any, error) {
		if !s.cfg.AllowShell {
			return nil, protocol.Errorf(protocol.CodeForbidden, "device.shell is disabled (set allowShell: true in the agent config)")
		}
		sh, isSheller := d.(device.Sheller)
		if !isSheller {
			return nil, device.ErrNotSupported
		}
		out, err := sh.Shell(ctx, p.Serial, p.Command)
		return map[string]any{"output": out}, err
	}))

	r.Handle("app.install", withDevice(s, func(ctx context.Context, p installParams, _ device.Info, d device.Driver) (any, error) {
		path := p.Path
		if p.URL != "" {
			tmp, err := download(ctx, p.URL, s.cfg.DataDir)
			if err != nil {
				return nil, err
			}
			defer os.Remove(tmp)
			path = tmp
		}
		if path == "" {
			return nil, protocol.Errorf(protocol.CodeBadRequest, "path or url is required")
		}
		if _, err := os.Stat(path); err != nil {
			return nil, protocol.Errorf(protocol.CodeBadRequest, "app file: %v", err)
		}
		return ok{true}, d.InstallApp(ctx, p.Serial, path)
	}))

	r.Handle("app.launch", withDevice(s, func(ctx context.Context, p appParams, _ device.Info, d device.Driver) (any, error) {
		return ok{true}, d.LaunchApp(ctx, p.Serial, p.AppID)
	}))

	r.Handle("app.terminate", withDevice(s, func(ctx context.Context, p appParams, _ device.Info, d device.Driver) (any, error) {
		return ok{true}, d.TerminateApp(ctx, p.Serial, p.AppID)
	}))

	// Raw WebDriverAgent passthrough, for anything the typed methods don't cover
	// (element find/click, alerts, orientation...).
	r.Handle("wda.request", withDevice(s, func(ctx context.Context, p rawHTTPParams, info device.Info, _ device.Driver) (any, error) {
		if info.Platform != device.IOS {
			return nil, fmt.Errorf("%w: wda.request is iOS only", device.ErrNotSupported)
		}
		c, err := s.ios.WDA(ctx, p.Serial)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(p.Path, "/") {
			return nil, protocol.Errorf(protocol.CodeBadRequest, "path must start with /")
		}
		var body []byte
		if len(p.Body) > 0 {
			body = p.Body
		}
		status, b, err := c.Raw(ctx, strings.ToUpper(p.Method), p.Path, body)
		if err != nil {
			return nil, protocol.Errorf(protocol.CodeUnavailable, "wda: %v", err)
		}
		return appium.BuildResponse(status, b), nil
	}))

	// WebDriver passthrough to the local Appium server. A remote test client can
	// implement a tiny HTTP->tunnel shim and drive sessions on this machine.
	r.Handle("appium.request", rpc.Typed(func(ctx context.Context, p appium.Request) (any, error) {
		return s.appium.Do(ctx, p)
	}))

	r.Handle("job.start", rpc.Typed(func(ctx context.Context, p jobs.Spec) (any, error) {
		return s.jobs.Start(p)
	}))
	r.Handle("job.list", rpc.Typed(func(ctx context.Context, _ struct{}) (any, error) {
		return s.jobs.List(), nil
	}))
	r.Handle("job.get", rpc.Typed(func(ctx context.Context, p jobIDParams) (any, error) {
		return s.jobs.Get(p.ID)
	}))
	r.Handle("job.cancel", rpc.Typed(func(ctx context.Context, p jobIDParams) (any, error) {
		return s.jobs.Cancel(p.ID)
	}))
	r.Handle("job.logs", rpc.Typed(func(ctx context.Context, p jobIDParams) (any, error) {
		return s.jobs.Logs(p.ID, p.Since, p.Limit)
	}))
	r.Handle("job.artifacts", rpc.Typed(func(ctx context.Context, p jobIDParams) (any, error) {
		return s.jobs.Artifacts(p.ID)
	}))
	r.Handle("job.artifact", rpc.Typed(func(ctx context.Context, p jobIDParams) (any, error) {
		b, err := s.jobs.ReadArtifact(p.ID, p.Name)
		if err != nil {
			return nil, err
		}
		return map[string]any{"name": p.Name, "data": base64.StdEncoding.EncodeToString(b)}, nil
	}))
}

// download fetches an app build into dir and returns the temp file path.
func download(ctx context.Context, rawURL, dir string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", protocol.Errorf(protocol.CodeBadRequest, "url must be http(s)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Minute}).Do(req)
	if err != nil {
		return "", protocol.Errorf(protocol.CodeUnavailable, "download: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", protocol.Errorf(protocol.CodeUnavailable, "download: HTTP %s", resp.Status)
	}
	ext := strings.ToLower(filepath.Ext(u.Path))
	switch ext {
	case ".apk", ".aab", ".ipa", ".app", ".zip":
	default:
		ext = ".bin"
	}
	f, err := os.CreateTemp(dir, "app-*"+ext)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, io.LimitReader(resp.Body, 4<<30)); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), f.Close()
}

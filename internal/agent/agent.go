// Package agent wires drivers, jobs, the Appium proxy and both transports
// (local HTTP API and the outbound tunnel) into one service.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ababank/mobile-device-agent/internal/appium"
	"github.com/ababank/mobile-device-agent/internal/config"
	"github.com/ababank/mobile-device-agent/internal/device"
	"github.com/ababank/mobile-device-agent/internal/device/android"
	"github.com/ababank/mobile-device-agent/internal/device/ios"
	"github.com/ababank/mobile-device-agent/internal/events"
	"github.com/ababank/mobile-device-agent/internal/httpapi"
	"github.com/ababank/mobile-device-agent/internal/jobs"
	"github.com/ababank/mobile-device-agent/internal/proc"
	"github.com/ababank/mobile-device-agent/internal/protocol"
	"github.com/ababank/mobile-device-agent/internal/rpc"
	"github.com/ababank/mobile-device-agent/internal/tunnel"
)

type Service struct {
	cfg     config.Config
	log     *slog.Logger
	version string
	agentID string
	started time.Time

	bus     *events.Bus
	android *android.Driver
	ios     *ios.Driver
	devices *device.Manager
	jobs    *jobs.Manager
	appium  *appium.Proxy
	router  *rpc.Router
	tunnel  *tunnel.Client
}

func New(ctx context.Context, cfg config.Config, log *slog.Logger, version string) (*Service, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	id, err := config.EnsureSecret(cfg.DataDir, "agent-id", 8)
	if err != nil {
		return nil, fmt.Errorf("agent id: %w", err)
	}
	if cfg.APIToken == "" {
		if cfg.APIToken, err = config.EnsureSecret(cfg.DataDir, "api-token", 24); err != nil {
			return nil, fmt.Errorf("api token: %w", err)
		}
	}

	s := &Service{cfg: cfg, log: log, version: version, agentID: id, started: time.Now(), bus: events.NewBus(), router: rpc.NewRouter()}
	s.android = android.New(cfg.ADBPath, log.With("platform", "android"))
	s.ios = ios.New(ctx, ios.Options{
		Backend:               cfg.IOS.Backend,
		GoIOSPath:             cfg.IOS.GoIOSPath,
		StartTunnel:           cfg.IOS.StartTunnel,
		WDABasePort:           cfg.IOS.WDABasePort,
		WDADevicePort:         cfg.IOS.WDADevicePort,
		WDABundleID:           cfg.IOS.WDABundleID,
		WDATestRunnerBundleID: cfg.IOS.WDATestRunnerBundleID,
		WDAXCTestConfig:       cfg.IOS.WDAXCTestConfig,
		WDAProjectPath:        cfg.IOS.WDAProjectPath,
	}, log.With("platform", "ios"))
	s.devices = device.NewManager([]device.Driver{s.android, s.ios}, s.bus, log, time.Duration(cfg.PollIntervalSec)*time.Second)
	if s.appium, err = appium.New(cfg.Appium.URL); err != nil {
		return nil, err
	}
	s.jobs = jobs.NewManager(cfg.Runners, cfg.DataDir, s.jobEnv, s.bus, log)
	s.registerMethods()

	if cfg.ServerURL != "" {
		s.tunnel = &tunnel.Client{
			URL:         cfg.ServerURL,
			Token:       cfg.ServerToken,
			AgentID:     id,
			Router:      s.router,
			Bus:         s.bus,
			Hello:       s.hello,
			MaxInFlight: cfg.MaxConcurrentRequests,
			Log:         log.With("component", "tunnel"),
		}
	}
	return s, nil
}

// Run blocks until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	go s.devices.Run(ctx)

	if s.cfg.Appium.AutoStart {
		port := "4723"
		if _, p, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(s.cfg.Appium.URL, "http://"), "https://")); err == nil {
			port = strings.SplitN(p, "/", 2)[0]
		}
		args := s.cfg.Appium.Args
		if len(args) == 0 {
			args = []string{"--port", port}
		}
		sup := &proc.Supervisor{Name: "appium", Path: s.cfg.Appium.Command, Args: args, Log: s.log.With("component", "appium")}
		go sup.Run(ctx)
	}

	if s.tunnel != nil {
		go s.tunnel.Run(ctx)
	} else {
		s.log.Info("no serverUrl configured; running local API only")
	}

	api := &httpapi.Server{
		Token:          s.cfg.APIToken,
		AllowedOrigins: s.cfg.AllowedOrigins,
		Router:         s.router,
		Bus:            s.bus,
		Appium:         s.appium.ReverseProxy("/appium"),
		Screenshot: func(ctx context.Context, serial string) ([]byte, error) {
			_, d, err := s.devices.Get(serial)
			if err != nil {
				return nil, err
			}
			return d.Screenshot(ctx, serial)
		},
		Source: func(ctx context.Context, serial string) (string, error) {
			_, d, err := s.devices.Get(serial)
			if err != nil {
				return "", err
			}
			return d.Source(ctx, serial)
		},
		Log: s.log,
	}
	srv := &http.Server{Addr: s.cfg.ListenAddr, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.ListenAddr, err)
	}
	s.log.Info("local API listening", "addr", "http://"+ln.Addr().String(), "tokenFile", filepath.Join(s.cfg.DataDir, "api-token"))

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func (s *Service) hello() protocol.Hello {
	host, _ := os.Hostname()
	return protocol.Hello{
		Protocol: protocol.Version,
		AgentID:  s.agentID,
		Name:     s.cfg.AgentName,
		Version:  s.version,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Hostname: host,
		Methods:  s.router.Methods(),
		Runners:  s.jobs.Runners(),
		Devices:  s.devices.List(),
	}
}

func (s *Service) jobEnv(serial string) (map[string]string, error) {
	info, d, err := s.devices.Get(serial)
	if err != nil {
		return nil, err
	}
	env := map[string]string{
		"DEVICE_SERIAL":   serial,
		"DEVICE_UDID":     serial,
		"DEVICE_PLATFORM": string(info.Platform),
		"DEVICE_NAME":     info.Name,
		"DEVICE_OS":       info.OSVersion,
		"APPIUM_URL":      s.appium.URL(),
	}
	if p, ok := d.(device.EnvProvider); ok {
		for k, v := range p.JobEnv(serial) {
			env[k] = v
		}
	}
	return env, nil
}

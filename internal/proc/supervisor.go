// Package proc keeps long-running helper processes (iproxy, WDA runner,
// go-ios tunnel, Appium) alive, restarting them with backoff.
package proc

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ababank/mobile-device-agent/internal/execx"
)

type Supervisor struct {
	Name string
	Path string
	Args []string
	Env  []string
	Log  *slog.Logger
}

// Run blocks until ctx is cancelled, restarting the process whenever it exits.
func (s *Supervisor) Run(ctx context.Context) {
	backoff := time.Second
	for {
		start := time.Now()
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		s.Log.Warn("helper exited, restarting", "helper", s.Name, "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (s *Supervisor) runOnce(ctx context.Context) error {
	cmd := execx.Command(ctx, s.Path, s.Args...)
	execx.PrepareTree(cmd)
	cmd.WaitDelay = 5 * time.Second
	if len(s.Env) > 0 {
		cmd.Env = append(os.Environ(), s.Env...)
	}
	w := &logWriter{log: s.Log, name: s.Name}
	cmd.Stdout = w
	cmd.Stderr = w
	s.Log.Info("starting helper", "helper", s.Name, "cmd", s.Path, "args", strings.Join(s.Args, " "))
	return cmd.Run()
}

type logWriter struct {
	log  *slog.Logger
	name string
}

func (w *logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			w.log.Debug(line, "helper", w.name)
		}
	}
	return len(p), nil
}

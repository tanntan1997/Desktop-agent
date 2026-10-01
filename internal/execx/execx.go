// Package execx wraps os/exec with the platform details the agent needs:
// no console windows on Windows, process-tree cancellation, and errors that
// carry the tool's stderr.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Command builds an *exec.Cmd with platform attributes applied.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	applyPlatformAttrs(cmd)
	return cmd
}

// Output runs a command and returns stdout. On failure the error includes the
// command's stderr (or the tail of stdout when stderr is empty).
func Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := Command(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	label := filepath.Base(name)
	if len(args) > 0 {
		label += " " + args[0]
	}
	if ctx.Err() != nil {
		return stdout.Bytes(), fmt.Errorf("%s: %w", label, ctx.Err())
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = lastLine(stdout.String())
	}
	if msg != "" {
		return stdout.Bytes(), fmt.Errorf("%s: %v: %s", label, err, msg)
	}
	return stdout.Bytes(), fmt.Errorf("%s: %w", label, err)
}

// Lookup returns the first candidate resolvable as an executable, or "".
func Lookup(candidates ...string) string {
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

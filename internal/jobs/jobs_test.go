package jobs

import (
	"io"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/ababank/mobile-device-agent/internal/config"
	"github.com/ababank/mobile-device-agent/internal/events"
	"github.com/ababank/mobile-device-agent/internal/protocol"
)

func TestLineWriter(t *testing.T) {
	var got []string
	w := &lineWriter{emit: func(s string) { got = append(got, s) }}
	w.Write([]byte("one\r\ntw"))
	w.Write([]byte("o\nthree"))
	w.flush()
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func newTestManager(t *testing.T, runners map[string]config.Runner) *Manager {
	env := func(serial string) (map[string]string, error) {
		return map[string]string{"DEVICE_SERIAL": serial}, nil
	}
	return NewManager(runners, t.TempDir(), env, events.NewBus(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func waitDone(t *testing.T, m *Manager, id string) Snapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s, _ := m.Get(id)
		if s.EndedAt != nil {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return Snapshot{}
}

func TestJobRunsAndCapturesOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	m := newTestManager(t, map[string]config.Runner{"sh": {Command: "sh", Args: []string{"-c"}}})

	snap, err := m.Start(Spec{Serial: "S1", Runner: "sh", Args: []string{`echo "serial=$DEVICE_SERIAL"; echo err >&2; exit 3`}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(Spec{Serial: "S1", Runner: "sh", Args: []string{"true"}}); err == nil {
		// The first job may already have finished on a fast machine; only a
		// busy error is acceptable if it fails.
	} else if protocol.AsError(err).Code != protocol.CodeBusy {
		t.Fatalf("second start: %v", err)
	}

	done := waitDone(t, m, snap.ID)
	if done.Status != StatusFailed || done.ExitCode == nil || *done.ExitCode != 3 {
		t.Fatalf("status=%s exit=%v", done.Status, done.ExitCode)
	}
	logs, _ := m.Logs(snap.ID, 0, 0)
	var sawOut, sawErr bool
	for _, l := range logs {
		sawOut = sawOut || (l.Stream == "stdout" && l.Text == "serial=S1")
		sawErr = sawErr || (l.Stream == "stderr" && l.Text == "err")
	}
	if !sawOut || !sawErr {
		t.Fatalf("logs missing output: %+v", logs)
	}
	arts, _ := m.Artifacts(snap.ID)
	if len(arts) != 1 || arts[0].Name != "output.log" {
		t.Fatalf("artifacts = %+v", arts)
	}
	if _, err := m.ReadArtifact(snap.ID, "../../etc/passwd"); err == nil {
		t.Fatal("path traversal not rejected")
	}
}

func TestJobCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	m := newTestManager(t, map[string]config.Runner{"sh": {Command: "sh", Args: []string{"-c"}}})
	snap, err := m.Start(Spec{Runner: "sh", Args: []string{"sleep 30"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	m.Cancel(snap.ID)
	if done := waitDone(t, m, snap.ID); done.Status != StatusCanceled {
		t.Fatalf("status = %s", done.Status)
	}
}

func TestRunnerAllowList(t *testing.T) {
	m := newTestManager(t, map[string]config.Runner{})
	_, err := m.Start(Spec{Runner: "rm"})
	if protocol.AsError(err).Code != protocol.CodeForbidden {
		t.Fatalf("err = %v", err)
	}
}

// Package jobs runs allow-listed test commands (pytest, patrol, gradle,
// xcodebuild, ...) against a device, captures their output, and streams log
// lines as events.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ababank/mobile-device-agent/internal/config"
	"github.com/ababank/mobile-device-agent/internal/events"
	"github.com/ababank/mobile-device-agent/internal/execx"
	"github.com/ababank/mobile-device-agent/internal/protocol"
)

const (
	EventUpdated = "job.updated"
	EventLog     = "job.log"

	maxLogLines     = 10000
	maxJobsKept     = 200
	maxArtifactSize = 64 << 20
)

type Status string

const (
	StatusRunning  Status = "running"
	StatusPassed   Status = "passed"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"
	StatusError    Status = "error"
)

type Spec struct {
	Serial     string            `json:"serial,omitempty"`
	Runner     string            `json:"runner"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	TimeoutSec int               `json:"timeoutSec,omitempty"`
}

type Snapshot struct {
	ID        string     `json:"id"`
	Spec      Spec       `json:"spec"`
	Status    Status     `json:"status"`
	ExitCode  *int       `json:"exitCode,omitempty"`
	Error     string     `json:"error,omitempty"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	LastSeq   int64      `json:"lastSeq"`
}

type LogLine struct {
	JobID  string    `json:"jobId"`
	Seq    int64     `json:"seq"`
	Stream string    `json:"stream"`
	Text   string    `json:"text"`
	Time   time.Time `json:"time"`
}

type Artifact struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// DeviceEnv validates a device and returns env vars describing it.
type DeviceEnv func(serial string) (map[string]string, error)

type Manager struct {
	runners map[string]config.Runner
	dataDir string
	envFor  DeviceEnv
	bus     *events.Bus
	log     *slog.Logger

	mu    sync.Mutex
	jobs  map[string]*job
	order []string
	busy  map[string]string // serial -> job id
}

type job struct {
	mu       sync.Mutex
	snap     Snapshot
	logs     []LogLine
	cancel   context.CancelFunc
	canceled bool
	dir      string
	logFile  *os.File
}

func NewManager(runners map[string]config.Runner, dataDir string, envFor DeviceEnv, bus *events.Bus, log *slog.Logger) *Manager {
	return &Manager{
		runners: runners, dataDir: dataDir, envFor: envFor, bus: bus, log: log,
		jobs: map[string]*job{}, busy: map[string]string{},
	}
}

func (m *Manager) Runners() []string {
	out := make([]string, 0, len(m.runners))
	for n := range m.runners {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Start launches a job. Only one job may hold a given device at a time.
func (m *Manager) Start(spec Spec) (Snapshot, error) {
	r, ok := m.runners[spec.Runner]
	if !ok {
		return Snapshot{}, protocol.Errorf(protocol.CodeForbidden,
			"runner %q is not allow-listed on this agent (available: %s)", spec.Runner, strings.Join(m.Runners(), ", "))
	}
	devEnv := map[string]string{}
	if spec.Serial != "" {
		var err error
		if devEnv, err = m.envFor(spec.Serial); err != nil {
			return Snapshot{}, err
		}
	}

	id := newID()
	m.mu.Lock()
	if spec.Serial != "" {
		if other, busy := m.busy[spec.Serial]; busy {
			m.mu.Unlock()
			return Snapshot{}, protocol.Errorf(protocol.CodeBusy, "device %s is running job %s", spec.Serial, other)
		}
		m.busy[spec.Serial] = id
	}
	m.mu.Unlock()

	dir := filepath.Join(m.dataDir, "jobs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.release(spec.Serial)
		return Snapshot{}, err
	}
	logFile, _ := os.Create(filepath.Join(dir, "output.log"))

	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if spec.TimeoutSec > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(spec.TimeoutSec)*time.Second)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	j := &job{
		snap:    Snapshot{ID: id, Spec: spec, Status: StatusRunning, StartedAt: time.Now()},
		cancel:  cancel,
		dir:     dir,
		logFile: logFile,
	}

	m.mu.Lock()
	m.jobs[id] = j
	m.order = append(m.order, id)
	m.pruneLocked()
	m.mu.Unlock()

	env := os.Environ()
	add := func(kv map[string]string) {
		for k, v := range kv {
			env = append(env, k+"="+v)
		}
	}
	add(r.Env)
	add(devEnv)
	add(spec.Env)
	add(map[string]string{"JOB_ID": id, "ARTIFACTS_DIR": dir})

	args := append(append([]string{}, r.Args...), spec.Args...)
	cmd := execx.Command(ctx, r.Command, args...)
	execx.PrepareTree(cmd)
	cmd.WaitDelay = 10 * time.Second
	cmd.Dir = r.WorkDir
	cmd.Env = env
	stdout := &lineWriter{emit: func(s string) { m.appendLog(j, "stdout", s) }}
	stderr := &lineWriter{emit: func(s string) { m.appendLog(j, "stderr", s) }}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	m.log.Info("job starting", "job", id, "runner", spec.Runner, "serial", spec.Serial, "args", args)
	if err := cmd.Start(); err != nil {
		m.finish(j, cmd, err, ctx)
		return j.snapshot(), nil
	}
	m.bus.Publish(EventUpdated, j.snapshot())
	go func() {
		err := cmd.Wait()
		stdout.flush()
		stderr.flush()
		m.finish(j, cmd, err, ctx)
	}()
	return j.snapshot(), nil
}

func (m *Manager) finish(j *job, cmd *exec.Cmd, err error, ctx context.Context) {
	j.mu.Lock()
	now := time.Now()
	j.snap.EndedAt = &now
	var exitErr *exec.ExitError
	switch {
	case j.canceled:
		j.snap.Status = StatusCanceled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		j.snap.Status = StatusError
		j.snap.Error = fmt.Sprintf("timed out after %ds", j.snap.Spec.TimeoutSec)
	case err == nil:
		j.snap.Status = StatusPassed
	case errors.As(err, &exitErr):
		j.snap.Status = StatusFailed
	default:
		j.snap.Status = StatusError
		j.snap.Error = err.Error()
	}
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		j.snap.ExitCode = &code
	}
	if j.logFile != nil {
		j.logFile.Close()
	}
	j.cancel()
	snap := j.snap
	j.mu.Unlock()

	m.release(snap.Spec.Serial)
	m.log.Info("job finished", "job", snap.ID, "status", snap.Status, "error", snap.Error)
	m.bus.Publish(EventUpdated, snap)
}

func (m *Manager) release(serial string) {
	if serial == "" {
		return
	}
	m.mu.Lock()
	delete(m.busy, serial)
	m.mu.Unlock()
}

func (m *Manager) appendLog(j *job, stream, text string) {
	j.mu.Lock()
	j.snap.LastSeq++
	l := LogLine{JobID: j.snap.ID, Seq: j.snap.LastSeq, Stream: stream, Text: text, Time: time.Now()}
	j.logs = append(j.logs, l)
	if len(j.logs) > maxLogLines {
		j.logs = j.logs[len(j.logs)-maxLogLines:]
	}
	if j.logFile != nil {
		fmt.Fprintf(j.logFile, "%s [%s] %s\n", l.Time.Format(time.RFC3339Nano), stream, text)
	}
	j.mu.Unlock()
	m.bus.Publish(EventLog, l)
}

// pruneLocked forgets the oldest finished jobs from memory (files stay on disk).
func (m *Manager) pruneLocked() {
	for len(m.order) > maxJobsKept {
		dropped := false
		for i, id := range m.order {
			if j := m.jobs[id]; j.snapshot().EndedAt != nil {
				delete(m.jobs, id)
				m.order = append(m.order[:i], m.order[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			return
		}
	}
}

func (m *Manager) get(id string) (*job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, protocol.Errorf(protocol.CodeNotFound, "job %q not found", id)
	}
	return j, nil
}

func (m *Manager) Get(id string) (Snapshot, error) {
	j, err := m.get(id)
	if err != nil {
		return Snapshot{}, err
	}
	return j.snapshot(), nil
}

func (m *Manager) List() []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Snapshot, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		out = append(out, m.jobs[m.order[i]].snapshot())
	}
	return out
}

func (m *Manager) Cancel(id string) (Snapshot, error) {
	j, err := m.get(id)
	if err != nil {
		return Snapshot{}, err
	}
	j.mu.Lock()
	if j.snap.EndedAt == nil {
		j.canceled = true
		j.cancel()
	}
	j.mu.Unlock()
	return j.snapshot(), nil
}

// Logs returns buffered lines with Seq > since, at most limit (0 = all).
func (m *Manager) Logs(id string, since int64, limit int) ([]LogLine, error) {
	j, err := m.get(id)
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	i := sort.Search(len(j.logs), func(i int) bool { return j.logs[i].Seq > since })
	out := append([]LogLine(nil), j.logs[i:]...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Manager) Artifacts(id string) ([]Artifact, error) {
	j, err := m.get(id)
	if err != nil {
		return nil, err
	}
	var out []Artifact
	err = filepath.WalkDir(j.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(j.dir, p)
		out = append(out, Artifact{Name: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	return out, err
}

// ReadArtifact reads a file from the job's artifacts dir, refusing paths that
// escape it.
func (m *Manager) ReadArtifact(id, name string) ([]byte, error) {
	j, err := m.get(id)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(j.dir, filepath.FromSlash(name))
	rel, err := filepath.Rel(j.dir, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, protocol.Errorf(protocol.CodeBadRequest, "invalid artifact name %q", name)
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, protocol.Errorf(protocol.CodeNotFound, "artifact %q not found", name)
	}
	if st.Size() > maxArtifactSize {
		return nil, protocol.Errorf(protocol.CodeBadRequest, "artifact %q is %d bytes; limit is %d", name, st.Size(), maxArtifactSize)
	}
	return os.ReadFile(p)
}

func (j *job) snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snap
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// lineWriter turns a byte stream into complete lines.
type lineWriter struct {
	mu   sync.Mutex
	buf  []byte
	emit func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		w.emit(strings.TrimRight(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 64<<10 { // pathological line without newline
		w.emit(string(w.buf))
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emit(strings.TrimRight(string(w.buf), "\r"))
		w.buf = nil
	}
}

package autotest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"

	"github.com/ababank/mobile-device-agent/internal/hub"
	"github.com/ababank/mobile-device-agent/internal/protocol"
)

// Events published on the hub bus (agentId = the run's agent).
const (
	EventRunUpdated = "run.updated" // data: Run without the log
	EventRunLog     = "run.log"     // data: {runId, entry}
)

const runTimeout = 30 * time.Minute

// Service creates tests and executes runs on hub devices.
type Service struct {
	Hub    *hub.Hub
	Store  *Store
	LLM    LLM // nil disables exploration (replay still works)
	Config ExploreConfig
	Log    *slog.Logger

	// device is how runs reach a device; replaced in tests.
	device func(agentID, serial string) (Device, error)

	mu     sync.Mutex
	active map[string]*activeRun // run id ->
	busy   map[string]string     // agentId/serial -> run id
}

type activeRun struct {
	mu     sync.Mutex // guards run
	run    *Run
	cancel context.CancelFunc
	done   chan struct{}
}

func NewService(h *hub.Hub, store *Store, llm LLM, cfg ExploreConfig, log *slog.Logger) *Service {
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = DefaultMaxTurns
	}
	s := &Service{Hub: h, Store: store, LLM: llm, Config: cfg, Log: log,
		active: map[string]*activeRun{}, busy: map[string]string{}}
	s.device = func(agentID, serial string) (Device, error) {
		a := h.Get(agentID)
		if a == nil {
			return nil, protocol.Errorf(protocol.CodeNotFound, "agent %q is not connected", agentID)
		}
		return &hubDevice{agent: a, serial: serial}, nil
	}
	s.recoverInterrupted()
	return s
}

// recoverInterrupted marks runs left "running" by a previous process as errors.
func (s *Service) recoverInterrupted() {
	runs, _ := s.Store.Runs("")
	for _, r := range runs {
		if r.Status == StatusRunning || r.Status == StatusQueued {
			full, err := s.Store.GetRun(r.ID)
			if err != nil {
				continue
			}
			full.Status, full.Error = StatusError, "interrupted: the server restarted during the run"
			now := time.Now().UTC()
			full.EndedAt = &now
			_ = s.Store.SaveRun(full)
		}
	}
}

// ---- tests ----

func (s *Service) CreateTest(t *Test) (*Test, error) {
	if strings.TrimSpace(t.Prompt) == "" {
		return nil, protocol.Errorf(protocol.CodeBadRequest, "prompt is required")
	}
	if t.Name == "" {
		t.Name = firstLine(t.Prompt, 80)
	}
	now := time.Now().UTC()
	t.ID, t.CreatedAt, t.UpdatedAt = newID(), now, now
	return t, s.Store.SaveTest(t)
}

func (s *Service) UpdateTest(t *Test) (*Test, error) {
	old, err := s.Store.GetTest(t.ID)
	if err != nil {
		return nil, err
	}
	t.CreatedAt, t.UpdatedAt = old.CreatedAt, time.Now().UTC()
	if t.Steps == nil {
		t.Steps, t.StepsRunID, t.Verified = old.Steps, old.StepsRunID, old.Verified
	} else if !stepsEqual(t.Steps, old.Steps) {
		t.StepsRunID, t.Verified = "", false // edited by hand
	} else {
		t.StepsRunID, t.Verified = old.StepsRunID, old.Verified
	}
	return t, s.Store.SaveTest(t)
}

func stepsEqual(a, b []Step) bool {
	return fmt.Sprintf("%+v", a) == fmt.Sprintf("%+v", b)
}

// ---- runs ----

// StartExplore lets the model explore the test on a device and record steps.
// With verify, a passing exploration is immediately replayed; the steps are
// saved to the test either way, and Verified reflects the replay.
func (s *Service) StartExplore(testID, agentID, serial string, verify bool) (*Run, error) {
	if s.LLM == nil {
		return nil, protocol.Errorf(protocol.CodeUnavailable, "AI exploration is disabled on this server (started with -no-ai)")
	}
	return s.start(testID, agentID, serial, ModeExplore, "", func(ctx context.Context, rr *runRecorder, t *Test, x *Executor) {
		s.explore(ctx, rr, t, x, verify)
	})
}

// StartReplay runs the test's recorded steps without the model.
func (s *Service) StartReplay(testID, agentID, serial string) (*Run, error) {
	t, err := s.Store.GetTest(testID)
	if err != nil {
		return nil, err
	}
	if len(t.Steps) == 0 {
		return nil, protocol.Errorf(protocol.CodeBadRequest, "test has no recorded steps yet; explore it first")
	}
	return s.start(testID, agentID, serial, ModeReplay, "", func(ctx context.Context, rr *runRecorder, t *Test, x *Executor) {
		s.replay(ctx, rr, t.Steps, x)
	})
}

func (s *Service) Cancel(runID string) error {
	s.mu.Lock()
	ar := s.active[runID]
	s.mu.Unlock()
	if ar == nil {
		return protocol.Errorf(protocol.CodeNotFound, "run %q is not active", runID)
	}
	ar.cancel()
	<-ar.done
	return nil
}

// GetRun returns the live state of an active run, else the stored one.
func (s *Service) GetRun(id string) (*Run, error) {
	s.mu.Lock()
	ar := s.active[id]
	s.mu.Unlock()
	if ar != nil {
		return ar.snapshot(), nil
	}
	return s.Store.GetRun(id)
}

// SaveSteps replaces a test's steps with the script kept in an exploration.
func (s *Service) SaveSteps(runID string) (*Test, error) {
	r, err := s.Store.GetRun(runID)
	if err != nil {
		return nil, err
	}
	if r.Mode != ModeExplore {
		return nil, protocol.Errorf(protocol.CodeBadRequest, "only exploration runs record steps")
	}
	t, err := s.Store.GetTest(r.TestID)
	if err != nil {
		return nil, err
	}
	t.Steps = keptSteps(r)
	if len(t.Steps) == 0 {
		return nil, protocol.Errorf(protocol.CodeBadRequest, "run recorded no steps")
	}
	t.StepsRunID, t.Verified, t.UpdatedAt = r.ID, false, time.Now().UTC()
	return t, s.Store.SaveTest(t)
}

func keptSteps(r *Run) []Step {
	var out []Step
	for _, sr := range r.Steps {
		if sr.Kept {
			out = append(out, sr.Step)
		}
	}
	return out
}

type runFunc func(ctx context.Context, rr *runRecorder, t *Test, x *Executor)

func (s *Service) start(testID, agentID, serial, mode, parent string, fn runFunc) (*Run, error) {
	t, err := s.Store.GetTest(testID)
	if err != nil {
		return nil, err
	}
	dev, err := s.device(agentID, serial)
	if err != nil {
		return nil, err
	}
	key := agentID + "/" + serial
	run := &Run{ID: newID(), TestID: testID, Mode: mode, AgentID: agentID, Serial: serial,
		Status: StatusRunning, StartedAt: time.Now().UTC(), Steps: []StepResult{}, Log: []LogEntry{}, ParentRunID: parent}

	s.mu.Lock()
	if other := s.busy[key]; other != "" {
		s.mu.Unlock()
		return nil, protocol.Errorf(protocol.CodeBusy, "device %s is busy with run %s", serial, other)
	}
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	ar := &activeRun{run: run, cancel: cancel, done: make(chan struct{})}
	s.busy[key] = run.ID
	s.active[run.ID] = ar
	s.mu.Unlock()

	if err := s.Store.SaveRun(run); err != nil {
		s.release(key, run.ID)
		cancel()
		return nil, err
	}
	rr := &runRecorder{s: s, ar: ar}
	rr.publish()
	snap := ar.snapshot()

	go func() {
		defer close(ar.done)
		defer cancel()
		defer s.release(key, run.ID)
		defer func() {
			if p := recover(); p != nil {
				rr.finish(StatusError, "", fmt.Sprintf("internal error: %v", p))
			}
		}()
		x := &Executor{Dev: dev, Variables: t.Variables}
		if err := s.measure(ctx, x); err != nil {
			rr.finish(StatusError, "", "read screen size: "+err.Error())
			return
		}
		fn(ctx, rr, t, x)
	}()
	return snap, nil
}

func (s *Service) release(key, runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[key] == runID {
		delete(s.busy, key)
	}
	delete(s.active, runID)
}

// measure reads the screen size from a screenshot.
func (s *Service) measure(ctx context.Context, x *Executor) error {
	img, err := x.Dev.Screenshot(ctx)
	if err != nil {
		return err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(img))
	if err != nil {
		return err
	}
	x.Width, x.Height = cfg.Width, cfg.Height
	return nil
}

func (s *Service) explore(ctx context.Context, rr *runRecorder, t *Test, x *Executor, verify bool) {
	effort := s.Config.reasoningEffort()
	if effort == "" {
		effort = "n/a"
	}
	rr.log("info", fmt.Sprintf("Exploring with %s (reasoning effort %s) on %s", s.Config.Model, effort, rr.ar.run.Serial))
	e := &explorer{cfg: s.Config, llm: s.LLM, x: x, test: t, rec: rr}
	out, err := e.run(ctx)
	if err != nil {
		rr.finish(statusFor(ctx, err), "", err.Error())
		return
	}
	rr.markKept(out.Kept)
	status := StatusFailed
	if out.Passed {
		status = StatusPassed
	}
	if len(out.Steps) > 0 {
		// Reload so edits made while exploring (name, prompt) are kept.
		if fresh, err := s.Store.GetTest(t.ID); err == nil {
			t = fresh
		}
		t.Steps, t.StepsRunID, t.Verified, t.UpdatedAt = out.Steps, rr.ar.run.ID, false, time.Now().UTC()
		if err := s.Store.SaveTest(t); err != nil {
			rr.log("error", "save steps: "+err.Error())
		} else {
			rr.log("info", fmt.Sprintf("Saved %d steps to the test.", len(out.Steps)))
		}
	}
	if !(out.Passed && verify && len(out.Steps) > 0) {
		rr.finish(status, out.Summary, "")
		return
	}

	// Verify: replay the kept steps from scratch, as a child run.
	rr.log("info", "Verifying: replaying the recorded steps without the model...")
	child := &Run{ID: newID(), TestID: t.ID, Mode: ModeReplay, AgentID: rr.ar.run.AgentID, Serial: rr.ar.run.Serial,
		Status: StatusRunning, StartedAt: time.Now().UTC(), Steps: []StepResult{}, Log: []LogEntry{}, ParentRunID: rr.ar.run.ID}
	// Cancelling the child cancels the whole exploration (they share ctx).
	cr := &runRecorder{s: s, ar: &activeRun{run: child, cancel: rr.ar.cancel, done: rr.ar.done}}
	s.mu.Lock()
	s.active[child.ID] = cr.ar
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, child.ID)
		s.mu.Unlock()
	}()
	rr.ar.mu.Lock()
	rr.ar.run.VerifyRunID = child.ID
	rr.ar.mu.Unlock()
	_ = s.Store.SaveRun(child)
	cr.publish()

	s.replay(ctx, cr, out.Steps, x)
	verified := cr.status() == StatusPassed
	if verified {
		if fresh, err := s.Store.GetTest(t.ID); err == nil && fresh.StepsRunID == rr.ar.run.ID {
			fresh.Verified = true
			_ = s.Store.SaveTest(fresh)
		}
		rr.finish(StatusPassed, out.Summary+" Replay verified.", "")
	} else {
		rr.finish(StatusFailed, out.Summary, "exploration passed but replaying the recorded steps failed: "+cr.errorText())
	}
}

func (s *Service) replay(ctx context.Context, rr *runRecorder, steps []Step, x *Executor) {
	rr.log("info", fmt.Sprintf("Replaying %d steps on %s", len(steps), rr.ar.run.Serial))
	for i := range steps {
		st := steps[i]
		start := time.Now()
		rr.log("action", fmt.Sprintf("%d. %s", i+1, describeStep(&st)))
		err := x.Exec(ctx, &st)
		sr := StepResult{N: i + 1, Step: st, Status: StatusPassed, Kept: true,
			DurationMs: time.Since(start).Milliseconds(), Time: time.Now().UTC()}
		if err != nil {
			sr.Status, sr.Error = StatusFailed, err.Error()
			rr.step(sr)
			if shot, serr := x.Dev.Screenshot(ctx); serr == nil {
				if jpg, _, _, _, err := shrink(shot); err == nil {
					rr.screenshot(jpg)
				}
			}
			var se *StepError
			status := StatusFailed
			if !errors.As(err, &se) {
				status = statusFor(ctx, err)
			}
			rr.finish(status, "", fmt.Sprintf("step %d (%s) failed: %v", i+1, firstNonEmpty(st.Description, st.Action), err))
			return
		}
		rr.step(sr)
	}
	if shot, err := x.Dev.Screenshot(ctx); err == nil {
		if jpg, _, _, _, err := shrink(shot); err == nil {
			rr.screenshot(jpg)
		}
	}
	rr.finish(StatusPassed, fmt.Sprintf("All %d steps passed.", len(steps)), "")
}

func statusFor(ctx context.Context, err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return StatusCanceled
	}
	return StatusError
}

// ---- recorder: keeps the run state, persists it, publishes events ----

type runRecorder struct {
	s  *Service
	ar *activeRun
}

func (ar *activeRun) snapshot() *Run {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	r := *ar.run
	r.Steps = append([]StepResult(nil), ar.run.Steps...)
	r.Log = append([]LogEntry(nil), ar.run.Log...)
	return &r
}

func (rr *runRecorder) log(kind, text string) {
	e := LogEntry{Time: time.Now().UTC(), Kind: kind, Text: text}
	rr.ar.mu.Lock()
	rr.ar.run.Log = append(rr.ar.run.Log, e)
	rr.ar.mu.Unlock()
	rr.s.Hub.Publish(rr.ar.run.AgentID, EventRunLog, map[string]any{"runId": rr.ar.run.ID, "entry": e})
}

func (rr *runRecorder) step(sr StepResult) {
	rr.ar.mu.Lock()
	rr.ar.run.Steps = append(rr.ar.run.Steps, sr)
	rr.ar.mu.Unlock()
	rr.save()
}

func (rr *runRecorder) screenshot(jpg []byte) {
	rr.ar.mu.Lock()
	n := len(rr.ar.run.Steps)
	rr.ar.mu.Unlock()
	if n == 0 || jpg == nil {
		return
	}
	name := fmt.Sprintf("%d.jpg", n)
	p, err := rr.s.Store.RunFile(rr.ar.run.ID, name)
	if err == nil {
		err = os.WriteFile(p, jpg, 0o600)
	}
	if err != nil {
		rr.s.Log.Warn("save screenshot", "run", rr.ar.run.ID, "err", err)
		return
	}
	rr.ar.mu.Lock()
	rr.ar.run.Steps[n-1].Screenshot = name
	rr.ar.mu.Unlock()
	rr.save()
}

func (rr *runRecorder) usage(u openai.CompletionUsage) {
	rr.ar.mu.Lock()
	if rr.ar.run.Usage == nil {
		rr.ar.run.Usage = &Usage{}
	}
	us := rr.ar.run.Usage
	cached := u.PromptTokensDetails.CachedTokens
	us.Turns++
	us.InputTokens += u.PromptTokens - cached // uncached input
	us.CacheReadTokens += cached
	us.OutputTokens += u.CompletionTokens
	us.ReasoningTokens += u.CompletionTokensDetails.ReasoningTokens
	rr.ar.mu.Unlock()
}

// markKept flags the results of the recorded steps that made the final script.
func (rr *runRecorder) markKept(kept []int) {
	rr.ar.mu.Lock()
	for i := range rr.ar.run.Steps {
		sr := &rr.ar.run.Steps[i]
		for _, n := range kept {
			if sr.N == n && sr.Status == StatusPassed {
				sr.Kept = true
			}
		}
	}
	rr.ar.mu.Unlock()
}

func (rr *runRecorder) finish(status, summary, errText string) {
	now := time.Now().UTC()
	rr.ar.mu.Lock()
	r := rr.ar.run
	r.Status, r.Summary, r.Error, r.EndedAt = status, summary, errText, &now
	rr.ar.mu.Unlock()
	msg := status
	if summary != "" {
		msg += ": " + summary
	}
	if errText != "" {
		msg += " — " + errText
	}
	rr.log("info", "Run "+msg)
	rr.save()
}

func (rr *runRecorder) status() string {
	rr.ar.mu.Lock()
	defer rr.ar.mu.Unlock()
	return rr.ar.run.Status
}

func (rr *runRecorder) errorText() string {
	rr.ar.mu.Lock()
	defer rr.ar.mu.Unlock()
	return rr.ar.run.Error
}

func (rr *runRecorder) save() {
	snap := rr.ar.snapshot()
	if err := rr.s.Store.SaveRun(snap); err != nil {
		rr.s.Log.Warn("save run", "run", snap.ID, "err", err)
	}
	rr.publishSnap(snap)
}

func (rr *runRecorder) publish() { rr.publishSnap(rr.ar.snapshot()) }

func (rr *runRecorder) publishSnap(snap *Run) {
	snap.Log = nil
	rr.s.Hub.Publish(snap.AgentID, EventRunUpdated, snap)
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, n)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

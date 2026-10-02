package autotest

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

const defaultMaxSwipes = 8

// Timing; variables so tests can shorten them.
var (
	defaultTimeout = 10 * time.Second       // waiting for a target / assertion
	pollEvery      = 500 * time.Millisecond // hierarchy polling while waiting
	settle         = 800 * time.Millisecond // UI reaction time after an action
)

// Executor performs steps on a device. Width/Height are the screen size in
// pixels (needed for fractional points and iOS point scaling).
type Executor struct {
	Dev       Device
	Width     int
	Height    int
	Variables map[string]string
}

// StepError is a test failure (as opposed to an infrastructure error).
type StepError struct{ Msg string }

func (e *StepError) Error() string { return e.Msg }

func failf(format string, a ...any) error { return &StepError{Msg: fmt.Sprintf(format, a...)} }

var varRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Expand substitutes ${NAME} from the test variables, then the environment.
func (x *Executor) Expand(s string) (string, error) {
	var missing []string
	out := varRef.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		if v, ok := x.Variables[name]; ok {
			return v
		}
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		missing = append(missing, name)
		return m
	})
	if len(missing) > 0 {
		return "", failf("undefined variable(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// Tree fetches and parses the current UI hierarchy.
func (x *Executor) Tree(ctx context.Context) (*Tree, error) {
	src, err := x.Dev.Source(ctx)
	if err != nil {
		return nil, err
	}
	return ParseTree(src, x.Width, 0)
}

// WaitFor polls the hierarchy until l resolves (or, with absent, until it
// does not) or the timeout passes.
func (x *Executor) WaitFor(ctx context.Context, l *Locator, timeout time.Duration, absent bool) (*Element, *Tree, error) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		t, err := x.Tree(ctx)
		if err == nil {
			e := t.Resolve(l)
			if (e != nil) != absent {
				return e, t, nil
			}
		} else {
			lastErr = err
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			if lastErr != nil && t == nil {
				return nil, nil, lastErr
			}
			if absent {
				return nil, t, failf("%s is still visible after %s", l, timeout)
			}
			return nil, t, failf("%s not found after %s", l, timeout)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

func (x *Executor) px(p *Point) (int, int) {
	return int(p.X * float64(x.Width)), int(p.Y * float64(x.Height))
}

// swipeFor returns finger start/end points for a direction of finger movement.
func swipeFor(dir string) (from, to Point, ok bool) {
	switch dir {
	case "up":
		return Point{0.5, 0.75}, Point{0.5, 0.3}, true
	case "down":
		return Point{0.5, 0.3}, Point{0.5, 0.75}, true
	case "left":
		return Point{0.85, 0.5}, Point{0.15, 0.5}, true
	case "right":
		return Point{0.15, 0.5}, Point{0.85, 0.5}, true
	}
	return Point{}, Point{}, false
}

// contentDir maps "where the content is" (scroll_to) to the finger movement
// that reveals it: content below needs the finger to move up.
var contentDir = map[string]string{"down": "up", "up": "down", "right": "left", "left": "right"}

// Exec runs one step. It returns a *StepError for test failures (element
// missing, assertion false) and other errors for device/infrastructure
// problems.
func (x *Executor) Exec(ctx context.Context, s *Step) error {
	timeout := time.Duration(s.TimeoutMs) * time.Millisecond
	acted := true
	switch s.Action {
	case ActLaunchApp:
		if s.AppID == "" {
			return failf("launch_app needs appId")
		}
		_ = x.Dev.Terminate(ctx, s.AppID) // fresh start; fine if it wasn't running
		if err := x.Dev.Launch(ctx, s.AppID); err != nil {
			return err
		}
		sleep(ctx, 2*settle)
	case ActTerminateApp:
		if err := x.Dev.Terminate(ctx, s.AppID); err != nil {
			return err
		}
	case ActTap:
		var px, py int
		switch {
		case !s.Target.Empty():
			e, _, err := x.WaitFor(ctx, s.Target, timeout, false)
			if err != nil {
				return err
			}
			px, py = e.Bounds.Center()
		case s.Point != nil:
			px, py = x.px(s.Point)
		default:
			return failf("tap needs a target or point")
		}
		if err := x.Dev.Tap(ctx, px, py); err != nil {
			return err
		}
	case ActInputText:
		if !s.Target.Empty() {
			e, _, err := x.WaitFor(ctx, s.Target, timeout, false)
			if err != nil {
				return err
			}
			if !e.Focused {
				cx, cy := e.Bounds.Center()
				if err := x.Dev.Tap(ctx, cx, cy); err != nil {
					return err
				}
				sleep(ctx, 400*time.Millisecond)
			}
		}
		text, err := x.Expand(s.Text)
		if err != nil {
			return err
		}
		if err := x.Dev.Text(ctx, text); err != nil {
			return err
		}
	case ActSwipe:
		from, to, ok := swipeFor(s.Direction)
		if s.Point != nil && s.To != nil {
			from, to, ok = *s.Point, *s.To, true
		}
		if !ok {
			return failf("swipe needs direction up/down/left/right or point+to")
		}
		x1, y1 := x.px(&from)
		x2, y2 := x.px(&to)
		if err := x.Dev.Swipe(ctx, x1, y1, x2, y2, max(s.DurationMs, 300)); err != nil {
			return err
		}
	case ActScrollTo:
		if s.Target.Empty() {
			return failf("scroll_to needs a target")
		}
		finger, ok := contentDir[s.Direction]
		if !ok {
			finger = "up"
		}
		from, to, _ := swipeFor(finger)
		x1, y1 := x.px(&from)
		x2, y2 := x.px(&to)
		n := s.MaxSwipes
		if n <= 0 {
			n = defaultMaxSwipes
		}
		for i := 0; ; i++ {
			t, err := x.Tree(ctx)
			if err != nil {
				return err
			}
			if t.Resolve(s.Target) != nil {
				break
			}
			if i == n {
				return failf("%s not found after %d swipes", s.Target, n)
			}
			if err := x.Dev.Swipe(ctx, x1, y1, x2, y2, 400); err != nil {
				return err
			}
			sleep(ctx, settle)
		}
		acted = false
	case ActPressKey:
		if err := x.Dev.Key(ctx, s.Key); err != nil {
			return err
		}
	case ActWait:
		sleep(ctx, time.Duration(max(s.DurationMs, 0))*time.Millisecond)
		acted = false
	case ActAssertVisible:
		if s.Target.Empty() {
			return failf("assert_visible needs a target")
		}
		if _, _, err := x.WaitFor(ctx, s.Target, timeout, false); err != nil {
			return err
		}
		acted = false
	case ActAssertNotVisible:
		if s.Target.Empty() {
			return failf("assert_not_visible needs a target")
		}
		if _, _, err := x.WaitFor(ctx, s.Target, timeout, true); err != nil {
			return err
		}
		acted = false
	default:
		return failf("unknown action %q", s.Action)
	}
	if acted {
		sleep(ctx, settle)
	}
	return ctx.Err()
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (l *Locator) String() string {
	if l == nil {
		return "<none>"
	}
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, fmt.Sprintf("%s=%q", k, v))
		}
	}
	add("id", l.ID)
	add("text", l.Text)
	add("textContains", l.TextContains)
	add("desc", l.Desc)
	add("class", l.Class)
	if l.Index > 0 {
		parts = append(parts, fmt.Sprintf("index=%d", l.Index))
	}
	return "element{" + strings.Join(parts, " ") + "}"
}

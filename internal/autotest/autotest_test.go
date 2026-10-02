package autotest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ababank/mobile-device-agent/internal/hub"
)

func init() {
	settle, pollEvery, defaultTimeout = 10*time.Millisecond, 10*time.Millisecond, 500*time.Millisecond
}

func TestParseAndLocate(t *testing.T) {
	p := newFakePhone(&testing.T{})
	p.screen = "dashboard"
	tree, err := ParseTree(p.xml(), fakeW, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Width != fakeW || tree.Height != fakeH {
		t.Fatalf("size %dx%d", tree.Width, tree.Height)
	}
	ex := tree.Find(&Locator{Text: "Exchange"})
	if len(ex) != 1 {
		t.Fatalf("find Exchange: %v", ex)
	}
	// The resource-id is unique, so it wins over the text.
	if l := tree.LocatorFor(ex[0]); l.ID != fakeApp+":id/menu_exchange" || l.Text != "" {
		t.Fatalf("locator %+v", l)
	}
	// Dynamic text without an id falls back to text.
	bal := tree.Find(&Locator{TextContains: "balance"})
	if len(bal) != 1 || tree.LocatorFor(bal[0]).Text != "Balance 1,234.56 USD" {
		t.Fatalf("balance %v", bal)
	}
	desc := tree.Describe(0.5, 100, func(s string) string { return s })
	if !strings.Contains(desc, `Button`) && !strings.Contains(desc, `"Exchange" id=menu_exchange at(390,925)`) {
		t.Fatalf("describe:\n%s", desc)
	}
}

func TestParseIOSScalesPointsToPixels(t *testing.T) {
	src := `<?xml version="1.0"?><XCUIElementTypeApplication type="XCUIElementTypeApplication" name="MBB" label="MBB" x="0" y="0" width="390" height="844" visible="true">
<XCUIElementTypeButton type="XCUIElementTypeButton" name="exchange_menu" label="Exchange" x="200" y="700" width="100" height="40" visible="true" enabled="true"/>
<XCUIElementTypeStaticText type="XCUIElementTypeStaticText" name="Hidden" label="Hidden" x="0" y="0" width="10" height="10" visible="false"/>
</XCUIElementTypeApplication>`
	tree, err := ParseTree(src, 1170, 0) // 3x display
	if err != nil {
		t.Fatal(err)
	}
	e := tree.Resolve(&Locator{ID: "exchange_menu"})
	if e == nil || e.Bounds != (Rect{600, 2100, 900, 2220}) || !e.Clickable || e.Text != "Exchange" {
		t.Fatalf("got %+v", e)
	}
	if tree.Resolve(&Locator{Text: "Hidden"}) != nil {
		t.Fatal("invisible element should be skipped")
	}
}

func newTestService(t *testing.T, phone *fakePhone, llm LLM) *Service {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewService(hub.New(log), store, llm, ExploreConfig{}, log)
	s.device = func(agentID, serial string) (Device, error) { return phone, nil }
	return s
}

func wait(t *testing.T, s *Service, runID string) *Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, err := s.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if r.EndedAt != nil {
			s.mu.Lock()
			_, active := s.active[runID]
			s.mu.Unlock()
			if !active {
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run did not finish")
	return nil
}

func TestExploreRecordsVerifiesAndReplays(t *testing.T) {
	phone := newFakePhone(t)
	llm := &scriptedLLM{t: t}
	llm.turns = []func(string) []call{
		func(req string) []call { // detour: tap the launcher icon first
			return []call{{"tap", fmt.Sprintf(`{"element":%d,"description":"Open MBB from the launcher"}`, elementOf(t, req, `"MBB"`))}}
		},
		func(req string) []call {
			return []call{{"launch_app", `{"app_id":"` + fakeApp + `","description":"Launch MBB"}`}}
		},
		func(req string) []call {
			pin := elementOf(t, req, "id=pin")
			return []call{
				{"input_text", fmt.Sprintf(`{"element":%d,"text":"${PIN}","description":"Enter PIN"}`, pin)},
				{"tap", fmt.Sprintf(`{"element":%d,"description":"Tap Login"}`, elementOf(t, req, `"Login"`))},
			}
		},
		func(req string) []call { // a failed check the model then recovers from
			return []call{{"assert_visible", `{"text":"Exchanges","description":"Exchange menu is visible"}`}}
		},
		func(req string) []call {
			return []call{{"assert_visible", fmt.Sprintf(`{"element":%d,"description":"Exchange menu is visible"}`, elementOf(t, req, `"Exchange"`))}}
		},
		func(req string) []call {
			return []call{{"finish", `{"result":"passed","summary":"Exchange menu is visible on the dashboard.","keep_steps":[2,3,4,5]}`}}
		},
	}
	s := newTestService(t, phone, llm)
	test, err := s.CreateTest(&Test{Prompt: "Verify Dashboard menu of exchange visible\n1. open MBB app\n2. login with PIN",
		AppID: fakeApp, Variables: map[string]string{"PIN": "1234"}})
	if err != nil {
		t.Fatal(err)
	}

	run, err := s.StartExplore(test.ID, "agent", "emu", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartReplay(test.ID, "agent", "emu"); err == nil {
		t.Fatal("expected an error starting a second run on a busy device")
	}
	r := wait(t, s, run.ID)
	if r.Status != StatusPassed || !strings.Contains(r.Summary, "Replay verified") {
		t.Fatalf("explore: %s %q %q\n%+v", r.Status, r.Summary, r.Error, r.Log)
	}
	if u := r.Usage; u == nil || u.Turns != 6 || u.InputTokens != 6000 || u.CacheReadTokens != 3000 || u.ReasoningTokens != 240 {
		t.Fatalf("usage %+v", r.Usage)
	}
	// Requests carry the OpenAI settings the explorer relies on.
	first := llm.requests[0]
	if strings.Contains(first, "reasoning_effort") {
		t.Fatal("reasoning_effort must not be sent to gpt-4o-mini")
	}
	for _, want := range []string{`"model":"gpt-4o-mini"`, `"store":false`,
		`"prompt_cache_key":"autotest-` + test.ID + `"`, `"detail":"high"`, `data:image/jpeg;base64,`} {
		if !strings.Contains(first, want) {
			t.Fatalf("request missing %s", want)
		}
	}
	// Only the latest screen is sent; earlier ones become placeholders, and
	// the test prompt in the first message survives.
	for i, req := range llm.requests {
		if n := strings.Count(req, "data:image/jpeg;base64,"); n != 1 {
			t.Fatalf("request %d has %d screenshots", i, n)
		}
	}
	if last := llm.requests[len(llm.requests)-1]; !strings.Contains(last, "Earlier screen omitted") ||
		!strings.Contains(last, "Verify Dashboard menu of exchange visible") {
		t.Fatal("old observations not collapsed, or the prompt was lost")
	}
	// Every tool call is answered by a tool message, and the new screen follows.
	if last := llm.requests[len(llm.requests)-1]; !strings.Contains(last, `"tool_call_id":"call_3_1"`) ||
		!strings.Contains(last, "Current screen after your last action") {
		t.Fatal("tool results / observation not sent back")
	}
	// The failed assertion is in the run but not in the script.
	var failed, kept int
	for _, sr := range r.Steps {
		if sr.Status == StatusFailed {
			failed++
		}
		if sr.Kept {
			kept++
		}
	}
	if failed != 1 || kept != 4 {
		t.Fatalf("failed=%d kept=%d: %+v", failed, kept, r.Steps)
	}

	saved, _ := s.Store.GetTest(test.ID)
	if !saved.Verified || saved.StepsRunID != run.ID || len(saved.Steps) != 4 {
		t.Fatalf("saved test: %+v", saved)
	}
	got := []string{}
	for _, st := range saved.Steps {
		got = append(got, st.Action)
	}
	if strings.Join(got, ",") != "launch_app,input_text,tap,assert_visible" {
		t.Fatalf("steps %v", got)
	}
	if st := saved.Steps[1]; st.Text != "${PIN}" || st.Target.ID != fakeApp+":id/pin" {
		t.Fatalf("input step %+v", st)
	}
	// Assertions on an element also pin its label.
	if st := saved.Steps[3]; st.Target.ID != fakeApp+":id/menu_exchange" || st.Target.Text != "Exchange" {
		t.Fatalf("assert step %+v", st.Target)
	}
	// The PIN value never reached the model.
	for i, req := range llm.requests {
		if strings.Contains(req, "1234") {
			t.Fatalf("request %d leaked the PIN", i)
		}
	}
	if r.VerifyRunID == "" {
		t.Fatal("no verify run")
	}
	if v, _ := s.GetRun(r.VerifyRunID); v.Status != StatusPassed || len(v.Steps) != 4 {
		t.Fatalf("verify run %+v", v)
	}

	// A later replay runs without the model.
	calls := len(llm.requests)
	phone.Terminate(context.Background(), fakeApp)
	rep, err := s.StartReplay(test.ID, "agent", "emu")
	if err != nil {
		t.Fatal(err)
	}
	if r := wait(t, s, rep.ID); r.Status != StatusPassed {
		t.Fatalf("replay %s %s", r.Status, r.Error)
	}
	if len(llm.requests) != calls {
		t.Fatal("replay called the model")
	}
}

func TestReplayFailsOnWrongPIN(t *testing.T) {
	phone := newFakePhone(t)
	s := newTestService(t, phone, nil)
	test, _ := s.CreateTest(&Test{Prompt: "login", Variables: map[string]string{"PIN": "0000"}})
	test.Steps = []Step{
		{Action: ActLaunchApp, AppID: fakeApp},
		{Action: ActInputText, Target: &Locator{ID: fakeApp + ":id/pin"}, Text: "${PIN}"},
		{Action: ActTap, Target: &Locator{Text: "Login"}},
		{Action: ActAssertVisible, Target: &Locator{Text: "Exchange"}, TimeoutMs: 300},
	}
	if err := s.Store.SaveTest(test); err != nil {
		t.Fatal(err)
	}
	run, err := s.StartReplay(test.ID, "a", "d")
	if err != nil {
		t.Fatal(err)
	}
	r := wait(t, s, run.ID)
	if r.Status != StatusFailed || !strings.Contains(r.Error, "step 4") || !strings.Contains(r.Error, "not found") {
		t.Fatalf("got %s %q", r.Status, r.Error)
	}
	if last := r.Steps[len(r.Steps)-1]; last.Screenshot == "" {
		t.Fatal("failure screenshot not saved")
	}
}

func TestExploreDisabledWithoutLLM(t *testing.T) {
	s := newTestService(t, newFakePhone(t), nil)
	test, _ := s.CreateTest(&Test{Prompt: "x"})
	if _, err := s.StartExplore(test.ID, "a", "d", false); err == nil {
		t.Fatal("expected error")
	}
}

func TestCancel(t *testing.T) {
	phone := newFakePhone(t)
	s := newTestService(t, phone, nil)
	test, _ := s.CreateTest(&Test{Prompt: "x"})
	test.Steps = []Step{{Action: ActWait, DurationMs: 60_000}}
	_ = s.Store.SaveTest(test)
	run, _ := s.StartReplay(test.ID, "a", "d")
	time.Sleep(50 * time.Millisecond)
	if err := s.Cancel(run.ID); err != nil {
		t.Fatal(err)
	}
	if r := wait(t, s, run.ID); r.Status != StatusCanceled && r.Status != StatusError {
		t.Fatalf("status %s", r.Status)
	}
}

func TestShrinkFitsModelLimits(t *testing.T) {
	jpg, k, w, h, err := shrink(newFakePhone(t).shot)
	if err != nil {
		t.Fatal(err)
	}
	iw, ih := int(float64(w)*k), int(float64(h)*k)
	if w != fakeW || h != fakeH || ih > maxImageEdge || iw*ih > maxImagePixels || len(jpg) == 0 {
		t.Fatalf("k=%v %dx%d -> %dx%d", k, w, h, iw, ih)
	}
}

func TestReasoningEffort(t *testing.T) {
	for _, c := range []struct{ model, effort, want string }{
		{"gpt-4o-mini", "", ""},
		{"gpt-4o", "", ""},
		{"gpt-5.5", "", "medium"},
		{"gpt-5-chat-latest", "", ""},
		{"o4-mini", "", "medium"},
		{"gpt-5.5", "high", "high"},
		{"gpt-5.5", "off", ""},
		{"my-azure-deployment", "low", "low"},
	} {
		if got := (ExploreConfig{Model: c.model, Effort: c.effort}).reasoningEffort(); got != c.want {
			t.Errorf("%s/%q: got %q want %q", c.model, c.effort, got, c.want)
		}
	}
}

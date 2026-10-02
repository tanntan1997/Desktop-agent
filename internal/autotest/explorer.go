package autotest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

// LLM is the subset of the OpenAI client the explorer uses (faked in tests).
type LLM interface {
	New(ctx context.Context, params openai.ChatCompletionNewParams) (*openai.ChatCompletion, error)
}

// ExploreConfig tunes the model loop.
type ExploreConfig struct {
	Model string // default gpt-4o-mini
	// Effort is the reasoning effort: none | minimal | low | medium | high |
	// xhigh. Empty means "auto": medium for reasoning models (gpt-5, o-series),
	// omitted for others such as gpt-4o-mini, which reject the parameter.
	// "off" always omits it.
	Effort   string
	MaxTurns int // model requests per exploration (default 60)
}

const (
	DefaultModel    = "gpt-4o-mini"
	DefaultEffort   = "" // auto, see ExploreConfig.Effort
	DefaultMaxTurns = 60
	maxElements     = 250
)

// recorder receives an exploration's progress.
type recorder interface {
	log(kind, text string)
	step(sr StepResult)
	screenshot(jpg []byte) // the screen after the most recent step
	usage(u openai.CompletionUsage)
}

// Outcome is how an exploration ended.
type Outcome struct {
	Passed  bool
	Summary string
	Steps   []Step // the final, replayable script
	Kept    []int  // 1-based numbers of the recorded steps that were kept
}

type explorer struct {
	cfg  ExploreConfig
	llm  LLM
	x    *Executor
	test *Test
	rec  recorder

	tree     *Tree   // latest observed hierarchy; element numbers refer to it
	k        float64 // screenshot pixels -> image pixels
	recorded []Step  // successful steps, numbered from 1
	lastShot []byte  // JPEG of the latest observation
}

const systemPrompt = `You are a senior mobile QA automation engineer. You operate a real phone through tools to carry out a test described in plain language, and every successful action you take is recorded as a step of an automated test that will later be replayed WITHOUT you. Your job is to (1) reach and verify the goal, and (2) leave behind a clean, minimal, reliable script.

How you see the device:
- Each observation has a screenshot and a list of UI elements from the accessibility hierarchy, one per line: "#<n> <Class> "<text>" id=<resource-id> desc="<description>" at(x,y) size WxH [flags]". Coordinates are in the screenshot's pixel space.
- Element numbers are only valid for the most recent observation. Every action returns a fresh observation.
- Banking apps often block screenshots (the image is black) - then rely on the element list. If the element list is empty but the screenshot shows content, the app may draw its own UI (Flutter without semantics, games); use x/y taps on the screenshot.

How to act:
- Prefer targeting an element number: the recorder turns it into a stable locator (resource-id, accessibility description or text). Use x/y only when no element represents the target.
- Start with launch_app when you know the app id, so replays begin from a fresh app start.
- To type, use input_text with the element number of the field. If the test provides variables (for example ${PIN}), type the reference literally - "${PIN}" - never guess or invent credentials. If a required credential is not provided, finish as failed and say which variable is needed.
- Use scroll_to (not repeated swipes) to bring an element that is off-screen into view; it records a robust "scroll until visible" step.
- Handle unexpected screens (permission prompts, onboarding, update or promo dialogs) the way a tester would, then continue.
- Wait briefly with wait only when the app is visibly loading.

How to verify:
- Every expected result in the test ("verify", "should", "visible", "displayed", "shows") needs an assertion: assert_visible / assert_not_visible. Assert on stable labels (menu names, titles), not on values that change between runs such as balances, dates or OTPs - use text_contains for partial matches.
- An assertion that fails means the app does not meet the expectation. Look again (the screen may still be loading, or the element may need scrolling) before concluding it is a real failure.

Finishing:
- Call finish exactly once, when the goal is verified (result "passed") or when it cannot be achieved or an expectation is not met (result "failed", with the reason in the summary).
- In finish, list keep_steps: the step numbers that form the clean script from a fresh app start to the final assertion. Leave out detours, mistakes and the actions that undid them. Omit keep_steps to keep every step.
- Keep each step's description short and in imperative form, like a manual test step: "Tap Exchange in the dashboard menu".`

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

var descProp = strProp("Short imperative description of this test step, e.g. \"Tap Login\".")

var locatorProps = map[string]any{
	"text":          strProp("Exact visible text of the element."),
	"text_contains": strProp("Case-insensitive part of the element's text or description."),
	"id":            strProp("Full resource-id / accessibility identifier."),
	"desc":          strProp("Exact accessibility description (content-desc)."),
}

func withProps(base map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func tool(name, desc string, props map[string]any, required ...string) openai.ChatCompletionToolUnionParam {
	params := shared.FunctionParameters{"type": "object", "properties": props}
	if len(required) > 0 {
		params["required"] = required
	}
	return openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
		Name:        name,
		Description: openai.String(desc),
		Parameters:  params,
	})
}

var directions = []string{"up", "down", "left", "right"}

var tools = []openai.ChatCompletionToolUnionParam{
	tool("observe", "Take a fresh screenshot and element list without acting. Use when the screen may have changed on its own.", map[string]any{}),
	tool("launch_app", "Start the app from a fresh state (it is stopped first if running).", map[string]any{
		"app_id": strProp("Android package (com.example.app) or iOS bundle id."), "description": descProp,
	}, "app_id", "description"),
	tool("tap", "Tap an element (preferred) or a screenshot coordinate.", map[string]any{
		"element":     intProp("Element number from the latest observation."),
		"x":           intProp("Screenshot x, only when no element fits."),
		"y":           intProp("Screenshot y, only when no element fits."),
		"description": descProp,
	}, "description"),
	tool("input_text", "Focus a text field (tap it) and type text. Use ${NAME} to type a test variable.", map[string]any{
		"element":     intProp("Element number of the field. Omit if the field is already focused."),
		"text":        strProp("Text to type, may contain ${NAME} variable references."),
		"description": descProp,
	}, "text", "description"),
	tool("swipe", "Swipe the screen. direction is the finger movement: \"up\" scrolls content down, \"left\" goes to the next page.", map[string]any{
		"direction": map[string]any{"type": "string", "enum": directions}, "description": descProp,
	}, "direction", "description"),
	tool("scroll_to", "Scroll until an element that is not on screen yet becomes visible. Identify it by text/text_contains/id/desc. direction is where the content is relative to the current view (default down).", withProps(locatorProps, map[string]any{
		"direction": map[string]any{"type": "string", "enum": directions}, "description": descProp,
	}), "description"),
	tool("press_key", "Press a device key.", map[string]any{
		"key":         map[string]any{"type": "string", "enum": []string{"back", "home", "enter", "delete", "app_switch"}},
		"description": descProp,
	}, "key", "description"),
	tool("wait", "Wait while the app is loading.", map[string]any{
		"seconds": map[string]any{"type": "number", "minimum": 0.5, "maximum": 30}, "description": descProp,
	}, "seconds", "description"),
	tool("assert_visible", "Check that an element is visible (waits up to 10s). Pass an element number, or text/text_contains/id/desc.", withProps(locatorProps, map[string]any{
		"element": intProp("Element number from the latest observation."), "description": descProp,
	}), "description"),
	tool("assert_not_visible", "Check that an element is not on screen (waits up to 10s for it to disappear).", withProps(locatorProps, map[string]any{
		"description": descProp,
	}), "description"),
	tool("finish", "End the exploration with the test verdict.", map[string]any{
		"result":     map[string]any{"type": "string", "enum": []string{"passed", "failed"}},
		"summary":    strProp("What was verified, or why the test failed."),
		"keep_steps": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Step numbers forming the clean script, in order. Omit to keep all."},
	}, "result", "summary"),
}

type toolInput struct {
	AppID        string  `json:"app_id"`
	Element      *int    `json:"element"`
	X            *int    `json:"x"`
	Y            *int    `json:"y"`
	Text         string  `json:"text"`
	TextContains string  `json:"text_contains"`
	ID           string  `json:"id"`
	Desc         string  `json:"desc"`
	Direction    string  `json:"direction"`
	Key          string  `json:"key"`
	Seconds      float64 `json:"seconds"`
	Description  string  `json:"description"`
	Result       string  `json:"result"`
	Summary      string  `json:"summary"`
	KeepSteps    []int   `json:"keep_steps"`
}

func (in *toolInput) locator() *Locator {
	return &Locator{ID: in.ID, Text: in.Text, TextContains: in.TextContains, Desc: in.Desc}
}

// reasoningEffort resolves the effort to send, or "" to omit it.
func (c ExploreConfig) reasoningEffort() string {
	switch c.Effort {
	case "off":
		return ""
	case "":
		if isReasoningModel(c.Model) {
			return "medium"
		}
		return ""
	}
	return c.Effort
}

// isReasoningModel reports whether an OpenAI model accepts reasoning_effort.
// Azure deployment names are arbitrary; set Effort explicitly there.
func isReasoningModel(model string) bool {
	m := strings.ToLower(model)
	if strings.HasPrefix(m, "gpt-5") {
		return !strings.Contains(m, "chat") // gpt-5-chat-latest is not a reasoning model
	}
	return len(m) > 1 && m[0] == 'o' && m[1] >= '1' && m[1] <= '9' // o1, o3, o4-mini, ...
}

// run drives the model until it calls finish.
func (e *explorer) run(ctx context.Context) (*Outcome, error) {
	first, err := e.observe(ctx)
	if err != nil {
		return nil, fmt.Errorf("initial observation: %w", err)
	}
	msgs := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
		openai.UserMessage(append([]openai.ChatCompletionContentPartUnionParam{openai.TextContentPart(e.intro())}, first...)),
	}
	// Only the latest screen stays in the conversation: a high-detail phone
	// screenshot is tens of thousands of tokens on gpt-4o-mini (128K context).
	// Older observations shrink to their lead text plus a placeholder.
	lastObs, lastObsText := 1, e.intro()
	addObservation := func(lead string, obs []openai.ChatCompletionContentPartUnionParam) {
		msgs[lastObs] = openai.UserMessage(lastObsText + "\n(Earlier screen omitted; see the latest observation.)")
		msgs = append(msgs, openai.UserMessage(append([]openai.ChatCompletionContentPartUnionParam{openai.TextContentPart(lead)}, obs...)))
		lastObs, lastObsText = len(msgs)-1, lead
	}
	nudged := false

	for turn := 1; turn <= e.cfg.MaxTurns; turn++ {
		params := openai.ChatCompletionNewParams{
			Model:               shared.ChatModel(e.cfg.Model),
			Messages:            msgs,
			Tools:               tools,
			MaxCompletionTokens: openai.Int(16000),
			// Screens of banking apps: don't keep completions on OpenAI's side.
			Store: openai.Bool(false),
			// Route every turn of a test to the same prompt cache.
			PromptCacheKey: openai.String("autotest-" + e.test.ID),
		}
		if effort := e.cfg.reasoningEffort(); effort != "" {
			params.ReasoningEffort = shared.ReasoningEffort(effort)
		}
		resp, err := e.llm.New(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("model request: %w", err)
		}
		e.rec.usage(resp.Usage)
		if len(resp.Choices) == 0 {
			return nil, errors.New("model returned no choices")
		}
		choice := resp.Choices[0]
		msg := choice.Message
		msgs = append(msgs, msg.ToParam())

		if msg.Refusal != "" {
			return nil, fmt.Errorf("model refused: %s", msg.Refusal)
		}
		if choice.FinishReason == "content_filter" {
			return nil, errors.New("model response was blocked by the content filter")
		}
		if s := strings.TrimSpace(msg.Content); s != "" {
			e.rec.log("say", s)
		}

		if len(msg.ToolCalls) == 0 {
			if choice.FinishReason == "length" {
				return nil, errors.New("model response hit the token limit without acting")
			}
			if nudged {
				return nil, errors.New("model stopped without calling finish")
			}
			nudged = true
			msgs = append(msgs, openai.UserMessage(
				"Continue the test using the tools. When you are done, call finish with the verdict."))
			continue
		}

		// Every tool call needs a tool message with its id. Tool messages can
		// only carry text, so the new screen follows as a user message.
		var outcome *Outcome
		observeAfter := false
		for _, c := range msg.ToolCalls {
			if c.Type != "function" {
				msgs = append(msgs, openai.ToolMessage("unsupported tool type "+c.Type, c.ID))
				continue
			}
			var in toolInput
			if err := json.Unmarshal([]byte(c.Function.Arguments), &in); err != nil {
				msgs = append(msgs, openai.ToolMessage("Error: invalid arguments: "+err.Error(), c.ID))
				continue
			}
			if c.Function.Name == "finish" {
				outcome = e.finish(&in)
				msgs = append(msgs, openai.ToolMessage("Recorded.", c.ID))
				continue
			}
			text, failed, err := e.act(ctx, c.Function.Name, &in)
			if err != nil {
				return nil, err // device or infrastructure problem
			}
			if failed && !strings.HasPrefix(text, "Failed") {
				text = "Error: " + text
			}
			msgs = append(msgs, openai.ToolMessage(text, c.ID))
			observeAfter = true
		}
		if outcome != nil {
			return outcome, nil
		}
		if observeAfter {
			obs, err := e.observe(ctx)
			if err != nil {
				return nil, err
			}
			e.rec.screenshot(e.lastShot)
			addObservation("Current screen after your last action:", obs)
		}
	}
	return nil, fmt.Errorf("no verdict after %d model turns", e.cfg.MaxTurns)
}

func (e *explorer) intro() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Test: %s\n\n%s\n\n", e.test.Name, strings.TrimSpace(e.test.Prompt))
	if e.test.AppID != "" {
		fmt.Fprintf(&b, "App id: %s\n", e.test.AppID)
	}
	if e.test.Platform != "" {
		fmt.Fprintf(&b, "Platform: %s\n", e.test.Platform)
	}
	if len(e.test.Variables) > 0 {
		names := make([]string, 0, len(e.test.Variables))
		for k := range e.test.Variables {
			names = append(names, "${"+k+"}")
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "Variables you can type (values are hidden from you): %s\n", strings.Join(names, ", "))
	}
	b.WriteString("\nThe current screen is below. Carry out the test.")
	return b.String()
}

// observe captures the screen and hierarchy as content for the model.
func (e *explorer) observe(ctx context.Context) ([]openai.ChatCompletionContentPartUnionParam, error) {
	png, err := e.x.Dev.Screenshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("screenshot: %w", err)
	}
	jpg, k, w, h, err := shrink(png)
	if err != nil {
		return nil, err
	}
	e.k, e.x.Width, e.x.Height = k, w, h
	e.lastShot = jpg

	var list string
	if t, err := e.x.Tree(ctx); err != nil {
		e.tree = &Tree{}
		list = "(UI hierarchy unavailable: " + err.Error() + ")\n"
	} else {
		e.tree = t
		list = t.Describe(k, maxElements, e.redact)
	}
	iw, ih := int(float64(w)*k), int(float64(h)*k)
	return []openai.ChatCompletionContentPartUnionParam{
		// "high" detail: phone UIs have small text that low detail loses.
		openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
			URL:    "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpg),
			Detail: "high",
		}),
		openai.TextContentPart(fmt.Sprintf("Screenshot %dx%d. Elements:\n%s", iw, ih, list)),
	}, nil
}

// redact hides variable values (e.g. a typed username) from the model.
func (e *explorer) redact(s string) string {
	for name, v := range e.test.Variables {
		if len(v) >= 3 {
			s = strings.ReplaceAll(s, v, "${"+name+"}")
		}
	}
	return s
}

// element returns the locator (or point) for an element number.
func (e *explorer) element(n int) (*Locator, *Point, error) {
	if e.tree == nil || n < 0 || n >= len(e.tree.Elements) {
		return nil, nil, fmt.Errorf("element #%d does not exist in the latest observation", n)
	}
	if l := e.tree.LocatorFor(n); l != nil {
		return l, nil, nil
	}
	cx, cy := e.tree.Elements[n].Bounds.Center()
	return nil, e.frac(cx, cy), nil
}

func (e *explorer) frac(px, py int) *Point {
	return &Point{X: float64(px) / float64(max(e.x.Width, 1)), Y: float64(py) / float64(max(e.x.Height, 1))}
}

// imagePoint converts a coordinate in the model's image to a fractional point.
func (e *explorer) imagePoint(x, y int) *Point {
	return e.frac(int(float64(x)/e.k), int(float64(y)/e.k))
}

// assertLocator strengthens an element locator for assertions: checking a
// menu by its id alone would pass even if its label were wrong.
func (e *explorer) assertLocator(n int, l *Locator) *Locator {
	el := e.tree.Elements[n]
	if l == nil || l.Text != "" || el.Text == "" || el.Password {
		return l
	}
	s := &Locator{ID: l.ID, Desc: l.Desc, Text: el.Text}
	for i, j := range e.tree.Find(s) {
		if j == n {
			s.Index = i
			return s
		}
	}
	return l
}

// act turns a tool call into a step, executes it, and records it on success.
// failed reports a test-level failure (shown to the model as a tool error);
// err is reserved for infrastructure errors that abort the run.
func (e *explorer) act(ctx context.Context, name string, in *toolInput) (msg string, failed bool, err error) {
	s := Step{Action: name, Description: in.Description}
	switch name {
	case "observe":
		return "Observation:", false, nil
	case "launch_app":
		s.AppID = in.AppID
	case "tap", "input_text":
		s.Text = in.Text
		switch {
		case in.Element != nil:
			l, p, err := e.element(*in.Element)
			if err != nil {
				return err.Error(), true, nil
			}
			s.Target, s.Point = l, p
		case name == "tap" && in.X != nil && in.Y != nil:
			s.Point = e.imagePoint(*in.X, *in.Y)
		case name == "tap":
			return "tap needs element or x and y", true, nil
		}
		if name == "input_text" && s.Target == nil && s.Point != nil {
			// No locator for the field: tap its position first, then type.
			tap := Step{Action: ActTap, Point: s.Point, Description: in.Description}
			if m, f, err := e.exec(ctx, &tap); err != nil || f {
				return m, f, err
			}
			s.Point = nil
		}
	case "swipe":
		s.Direction = in.Direction
	case "scroll_to":
		s.Target, s.Direction = in.locator(), in.Direction
		if s.Direction == "" {
			s.Direction = "down"
		}
	case "press_key":
		s.Key = in.Key
	case "wait":
		s.DurationMs = int(min(max(in.Seconds, 0.5), 30) * 1000)
	case "assert_visible", "assert_not_visible":
		if in.Element != nil {
			l, _, err := e.element(*in.Element)
			if err != nil {
				return err.Error(), true, nil
			}
			if l == nil {
				return "that element has no text or id to check; use text/text_contains instead", true, nil
			}
			s.Target = e.assertLocator(*in.Element, l)
		} else {
			s.Target = in.locator()
		}
		if s.Target.Empty() {
			return name + " needs an element or text/text_contains/id/desc", true, nil
		}
	default:
		return "unknown tool " + name, true, nil
	}
	return e.exec(ctx, &s)
}

func (e *explorer) exec(ctx context.Context, s *Step) (string, bool, error) {
	start := time.Now()
	e.rec.log("action", describeStep(s))
	err := e.x.Exec(ctx, s)
	var se *StepError
	if err != nil && !errors.As(err, &se) {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		// Device errors (e.g. app id not installed) are worth showing to the
		// model once rather than aborting; it can try another approach.
		se = &StepError{Msg: err.Error()}
	}
	sr := StepResult{Step: *s, Status: StatusPassed, DurationMs: time.Since(start).Milliseconds(), Time: time.Now().UTC()}
	if se != nil {
		sr.Status, sr.Error = StatusFailed, se.Msg
		e.rec.log("error", se.Msg)
		e.rec.step(sr)
		return "Failed: " + se.Msg, true, nil
	}
	e.recorded = append(e.recorded, *s)
	sr.N = len(e.recorded)
	e.rec.step(sr)
	return fmt.Sprintf("OK - recorded as step %d.", sr.N), false, nil
}

func (e *explorer) finish(in *toolInput) *Outcome {
	o := &Outcome{Passed: in.Result == "passed", Summary: in.Summary}
	keep := in.KeepSteps
	if len(keep) == 0 {
		for i := range e.recorded {
			keep = append(keep, i+1)
		}
	}
	for _, n := range keep {
		if n >= 1 && n <= len(e.recorded) && !slices.Contains(o.Kept, n) {
			o.Kept = append(o.Kept, n)
			o.Steps = append(o.Steps, e.recorded[n-1])
		}
	}
	return o
}

// describeStep renders a step for logs.
func describeStep(s *Step) string {
	d := s.Action
	switch {
	case !s.Target.Empty():
		d += " " + s.Target.String()
	case s.Point != nil:
		d += fmt.Sprintf(" at (%.2f, %.2f)", s.Point.X, s.Point.Y)
	}
	if s.AppID != "" {
		d += " " + s.AppID
	}
	if s.Text != "" {
		d += fmt.Sprintf(" text=%q", s.Text)
	}
	if s.Direction != "" {
		d += " " + s.Direction
	}
	if s.Key != "" {
		d += " " + s.Key
	}
	if s.DurationMs > 0 {
		d += fmt.Sprintf(" %dms", s.DurationMs)
	}
	if s.Description != "" {
		d += " — " + s.Description
	}
	return d
}

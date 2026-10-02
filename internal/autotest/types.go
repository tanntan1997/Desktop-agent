// Package autotest runs AI-driven mobile UI tests on hub devices.
//
// A test starts as a natural-language prompt. An exploration run lets an
// OpenAI model drive the device (screenshot + UI hierarchy in, tap/type/swipe out) until
// the goal is verified; every action it takes is recorded as a Step with a
// stable element Locator. Later runs replay those steps deterministically,
// without the model.
package autotest

import "time"

// Step actions.
const (
	ActLaunchApp        = "launch_app"    // AppID; terminates first for a clean start
	ActTerminateApp     = "terminate_app" // AppID
	ActTap              = "tap"           // Target or Point
	ActInputText        = "input_text"    // optional Target to focus, then Text
	ActSwipe            = "swipe"         // Direction (finger movement) or Point->To
	ActScrollTo         = "scroll_to"     // swipe until Target is visible; Direction = where the content is
	ActPressKey         = "press_key"     // Key
	ActWait             = "wait"          // DurationMs
	ActAssertVisible    = "assert_visible"
	ActAssertNotVisible = "assert_not_visible"
)

// Locator finds an element in the UI hierarchy. All non-empty fields must
// match; Index picks among several matches (document order).
type Locator struct {
	ID           string `json:"id,omitempty"`           // Android resource-id / iOS name
	Text         string `json:"text,omitempty"`         // exact visible text
	TextContains string `json:"textContains,omitempty"` // case-insensitive, text or desc
	Desc         string `json:"desc,omitempty"`         // Android content-desc / iOS value
	Class        string `json:"class,omitempty"`        // short class: Button, EditText, ...
	Index        int    `json:"index,omitempty"`
}

func (l *Locator) Empty() bool {
	return l == nil || (l.ID == "" && l.Text == "" && l.TextContains == "" && l.Desc == "" && l.Class == "")
}

// Point is a screen position as fractions (0..1) of width and height, so
// steps survive different screen resolutions.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Step is one replayable action or check.
type Step struct {
	Action      string   `json:"action"`
	Description string   `json:"description,omitempty"`
	Target      *Locator `json:"target,omitempty"`
	Point       *Point   `json:"point,omitempty"`
	To          *Point   `json:"to,omitempty"`
	Direction   string   `json:"direction,omitempty"` // up, down, left, right
	Text        string   `json:"text,omitempty"`      // may reference ${VAR}
	Key         string   `json:"key,omitempty"`
	AppID       string   `json:"appId,omitempty"`
	DurationMs  int      `json:"durationMs,omitempty"`
	TimeoutMs   int      `json:"timeoutMs,omitempty"` // wait for Target (default 10s)
	MaxSwipes   int      `json:"maxSwipes,omitempty"` // scroll_to (default 8)
}

// Test is a test case: the prompt plus the steps recorded from exploration.
type Test struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Prompt   string `json:"prompt"`
	AppID    string `json:"appId,omitempty"`    // package / bundle id hint for the model
	Platform string `json:"platform,omitempty"` // android | ios (informational)
	// Variables are substituted into input_text as ${NAME}. Their values are
	// never sent to the model.
	Variables  map[string]string `json:"variables,omitempty"`
	Steps      []Step            `json:"steps,omitempty"`
	StepsRunID string            `json:"stepsRunId,omitempty"` // run the steps were recorded in
	Verified   bool              `json:"verified"`             // steps passed a replay right after recording
	CreatedAt  time.Time         `json:"createdAt"`
	UpdatedAt  time.Time         `json:"updatedAt"`
}

// Run modes and statuses.
const (
	ModeExplore = "explore"
	ModeReplay  = "replay"

	StatusQueued   = "queued"
	StatusRunning  = "running"
	StatusPassed   = "passed"
	StatusFailed   = "failed"
	StatusError    = "error" // infrastructure / model problem, not a test verdict
	StatusCanceled = "canceled"
)

// Run is one exploration or replay of a test on one device.
type Run struct {
	ID        string       `json:"id"`
	TestID    string       `json:"testId"`
	Mode      string       `json:"mode"`
	AgentID   string       `json:"agentId"`
	Serial    string       `json:"serial"`
	Status    string       `json:"status"`
	Summary   string       `json:"summary,omitempty"`
	Error     string       `json:"error,omitempty"`
	Steps     []StepResult `json:"steps"`
	Log       []LogEntry   `json:"log"`
	Usage     *Usage       `json:"usage,omitempty"` // explore only
	StartedAt time.Time    `json:"startedAt"`
	EndedAt   *time.Time   `json:"endedAt,omitempty"`
	// VerifyRunID is the replay started automatically after a passing exploration.
	VerifyRunID string `json:"verifyRunId,omitempty"`
	ParentRunID string `json:"parentRunId,omitempty"`
}

// StepResult records one executed step.
type StepResult struct {
	N          int       `json:"n"` // 1-based
	Step       Step      `json:"step"`
	Status     string    `json:"status"` // passed | failed
	Error      string    `json:"error,omitempty"`
	Screenshot string    `json:"screenshot,omitempty"` // file name under the run's dir
	Kept       bool      `json:"kept"`                 // explore: part of the final script
	DurationMs int64     `json:"durationMs"`
	Time       time.Time `json:"time"`
}

// LogEntry is a line of the run's timeline (model reasoning, actions, errors).
type LogEntry struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // info | thinking | say | action | error
	Text string    `json:"text"`
}

// Usage accumulates model token usage over an exploration.
type Usage struct {
	Turns           int   `json:"turns"`
	InputTokens     int64 `json:"inputTokens"`     // uncached prompt tokens
	CacheReadTokens int64 `json:"cacheReadTokens"` // prompt tokens served from cache
	OutputTokens    int64 `json:"outputTokens"`    // includes reasoning tokens
	ReasoningTokens int64 `json:"reasoningTokens"`
}

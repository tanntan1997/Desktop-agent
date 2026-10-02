package autotest

import (
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Rect is an element's bounds in screenshot pixels.
type Rect struct{ X1, Y1, X2, Y2 int }

func (r Rect) W() int             { return r.X2 - r.X1 }
func (r Rect) H() int             { return r.Y2 - r.Y1 }
func (r Rect) Center() (int, int) { return (r.X1 + r.X2) / 2, (r.Y1 + r.Y2) / 2 }

// Element is a platform-neutral UI node.
type Element struct {
	Class      string // short: Button, TextView, StaticText, ...
	ID         string
	Text       string
	Desc       string
	Bounds     Rect
	Clickable  bool
	Enabled    bool
	Focused    bool
	Scrollable bool
	Checked    bool
	Password   bool
}

// Tree is a parsed hierarchy. Elements are in document order.
type Tree struct {
	Elements []Element
	Width    int // screen size in pixels (from the root bounds)
	Height   int
}

var androidBounds = regexp.MustCompile(`^\[(-?\d+),(-?\d+)\]\[(-?\d+),(-?\d+)\]$`)

// ParseTree parses uiautomator XML (Android) or WebDriverAgent XML (iOS).
// iOS coordinates are points; scale converts them to screenshot pixels (pass
// 0 to infer it from screenWidth and the root element's width).
func ParseTree(source string, screenWidth int, scale float64) (*Tree, error) {
	dec := xml.NewDecoder(strings.NewReader(source))
	dec.Strict = false
	t := &Tree{}
	ios := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse UI hierarchy: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		a := attrs(se)
		switch {
		case se.Name.Local == "node":
			e := Element{
				Class:      shortClass(a["class"]),
				ID:         a["resource-id"],
				Text:       strings.TrimSpace(a["text"]),
				Desc:       strings.TrimSpace(a["content-desc"]),
				Clickable:  a["clickable"] == "true" || a["long-clickable"] == "true",
				Enabled:    a["enabled"] != "false",
				Focused:    a["focused"] == "true",
				Scrollable: a["scrollable"] == "true",
				Checked:    a["checked"] == "true",
				Password:   a["password"] == "true",
			}
			if m := androidBounds.FindStringSubmatch(a["bounds"]); m != nil {
				e.Bounds = Rect{atoi(m[1]), atoi(m[2]), atoi(m[3]), atoi(m[4])}
			}
			if t.Width == 0 && e.Bounds.W() > 0 {
				t.Width, t.Height = e.Bounds.X2, e.Bounds.Y2
			}
			t.Elements = append(t.Elements, e)
		case strings.HasPrefix(se.Name.Local, "XCUIElementType"):
			ios = true
			if a["visible"] == "false" {
				continue
			}
			x, y, w, h := atof(a["x"]), atof(a["y"]), atof(a["width"]), atof(a["height"])
			if scale == 0 && w > 0 {
				scale = 1
				if screenWidth > 0 {
					scale = float64(screenWidth) / w
				}
			}
			s := max(scale, 1)
			e := Element{
				Class:    shortClass(se.Name.Local),
				ID:       a["name"],
				Text:     strings.TrimSpace(a["label"]),
				Desc:     strings.TrimSpace(a["value"]),
				Enabled:  a["enabled"] != "false",
				Focused:  a["focused"] == "true",
				Bounds:   Rect{int(x * s), int(y * s), int((x + w) * s), int((y + h) * s)},
				Password: se.Name.Local == "XCUIElementTypeSecureTextField",
			}
			switch e.Class {
			case "Button", "Cell", "Link", "Switch", "TextField", "SecureTextField", "SearchField", "Tab", "Image", "Icon", "MenuItem", "Key":
				e.Clickable = true
			}
			if e.Desc == e.Text {
				e.Desc = ""
			}
			if e.ID == e.Text {
				e.ID = "" // WDA mirrors the label into name when no identifier is set
			}
			if t.Width == 0 && e.Bounds.W() > 0 {
				t.Width, t.Height = e.Bounds.X2, e.Bounds.Y2
			}
			t.Elements = append(t.Elements, e)
		}
	}
	if ios && len(t.Elements) > 0 {
		// The Application element is the root; don't offer it as a target.
		t.Elements[0].Clickable = false
	}
	return t, nil
}

func attrs(se xml.StartElement) map[string]string {
	m := make(map[string]string, len(se.Attr))
	for _, a := range se.Attr {
		m[a.Name.Local] = a.Value
	}
	return m
}

func shortClass(c string) string {
	if i := strings.LastIndexByte(c, '.'); i >= 0 {
		c = c[i+1:]
	}
	return strings.TrimPrefix(c, "XCUIElementType")
}

func atoi(s string) int     { n, _ := strconv.Atoi(s); return n }
func atof(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
func shortID(id string) string {
	if i := strings.Index(id, ":id/"); i >= 0 {
		return id[i+4:]
	}
	return id
}

// Interesting reports whether an element is worth showing to the model: it
// is on screen and carries text, an id, or is interactive.
func (t *Tree) Interesting(e *Element) bool {
	if e.Bounds.W() <= 1 || e.Bounds.H() <= 1 {
		return false
	}
	if t.Width > 0 && (e.Bounds.X2 <= 0 || e.Bounds.Y2 <= 0 || e.Bounds.X1 >= t.Width || e.Bounds.Y1 >= t.Height) {
		return false
	}
	return e.Text != "" || e.Desc != "" || e.ID != "" || e.Clickable || e.Scrollable
}

// Find returns the elements matching l, in document order.
func (t *Tree) Find(l *Locator) []int {
	if l.Empty() {
		return nil
	}
	var out []int
	needle := strings.ToLower(l.TextContains)
	for i := range t.Elements {
		e := &t.Elements[i]
		if e.Bounds.W() <= 0 || e.Bounds.H() <= 0 {
			continue
		}
		if l.ID != "" && e.ID != l.ID {
			continue
		}
		if l.Text != "" && e.Text != l.Text {
			continue
		}
		if l.Desc != "" && e.Desc != l.Desc {
			continue
		}
		if l.Class != "" && e.Class != l.Class {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(e.Text), needle) && !strings.Contains(strings.ToLower(e.Desc), needle) {
			continue
		}
		out = append(out, i)
	}
	return out
}

// Resolve returns the element l selects, or nil.
func (t *Tree) Resolve(l *Locator) *Element {
	m := t.Find(l)
	if l.Index < len(m) {
		return &t.Elements[m[l.Index]]
	}
	return nil
}

// LocatorFor builds the most stable locator that selects element i: a unique
// id, then content description, then text, then combinations; finally the
// first available attribute plus an index. Text comes after id and desc
// because labels such as balances or dates change between runs. Returns nil
// if the element has nothing to anchor on.
func (t *Tree) LocatorFor(i int) *Locator {
	e := t.Elements[i]
	var cands []Locator
	if e.ID != "" {
		cands = append(cands, Locator{ID: e.ID})
	}
	if e.Desc != "" {
		cands = append(cands, Locator{Desc: e.Desc})
	}
	if e.Text != "" && !e.Password {
		cands = append(cands, Locator{Text: e.Text})
	}
	if e.ID != "" && e.Text != "" && !e.Password {
		cands = append(cands, Locator{ID: e.ID, Text: e.Text})
	}
	if e.ID != "" && e.Desc != "" {
		cands = append(cands, Locator{ID: e.ID, Desc: e.Desc})
	}
	if e.Text != "" && !e.Password {
		cands = append(cands, Locator{Class: e.Class, Text: e.Text})
	}
	if e.Desc != "" {
		cands = append(cands, Locator{Class: e.Class, Desc: e.Desc})
	}
	for _, c := range cands {
		if m := t.Find(&c); len(m) == 1 && m[0] == i {
			return &c
		}
	}
	if len(cands) == 0 {
		if e.Class == "" {
			return nil
		}
		cands = append(cands, Locator{Class: e.Class})
	}
	c := cands[0]
	for n, j := range t.Find(&c) {
		if j == i {
			c.Index = n
			return &c
		}
	}
	return nil
}

// Describe renders the interesting elements as compact lines for the model,
// with coordinates multiplied by k (screenshot pixels -> image pixels).
// redact replaces secret values that may appear in on-screen text.
func (t *Tree) Describe(k float64, limit int, redact func(string) string) string {
	var b strings.Builder
	n := 0
	for i := range t.Elements {
		e := &t.Elements[i]
		if !t.Interesting(e) {
			continue
		}
		if n == limit {
			fmt.Fprintf(&b, "... (more elements omitted)\n")
			break
		}
		n++
		fmt.Fprintf(&b, "#%d %s", i, e.Class)
		if e.Text != "" {
			txt := redact(e.Text)
			if e.Password {
				txt = "••••"
			}
			fmt.Fprintf(&b, " %q", truncate(txt, 80))
		}
		if e.ID != "" {
			fmt.Fprintf(&b, " id=%s", shortID(e.ID))
		}
		if e.Desc != "" {
			fmt.Fprintf(&b, " desc=%q", truncate(redact(e.Desc), 80))
		}
		cx, cy := e.Bounds.Center()
		fmt.Fprintf(&b, " at(%d,%d) size %dx%d", int(float64(cx)*k), int(float64(cy)*k),
			int(float64(e.Bounds.W())*k), int(float64(e.Bounds.H())*k))
		var flags []string
		if e.Clickable {
			flags = append(flags, "clickable")
		}
		if e.Scrollable {
			flags = append(flags, "scrollable")
		}
		if !e.Enabled {
			flags = append(flags, "disabled")
		}
		if e.Focused {
			flags = append(flags, "focused")
		}
		if e.Checked {
			flags = append(flags, "checked")
		}
		if len(flags) > 0 {
			fmt.Fprintf(&b, " [%s]", strings.Join(flags, ","))
		}
		b.WriteByte('\n')
	}
	if n == 0 {
		b.WriteString("(no elements in the UI hierarchy)\n")
	}
	return b.String()
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

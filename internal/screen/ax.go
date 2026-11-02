package screen

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"
)

// AXFrame is a native rectangle in desktop points.
type AXFrame struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// AXElement is one row of a native accessibility snapshot.
type AXElement struct {
	Index      int      `json:"index"`
	Parent     *int     `json:"parent"`
	Depth      int      `json:"depth"`
	Role       string   `json:"role"`
	Subrole    string   `json:"subrole"`
	Label      string   `json:"label"`
	Title      string   `json:"title"`
	Identifier string   `json:"identifier"`
	Value      string   `json:"value"`
	Enabled    *bool    `json:"enabled"`
	Focused    *bool    `json:"focused"`
	Selected   *bool    `json:"selected"`
	Secure     bool     `json:"secure"`
	Frame      *AXFrame `json:"frame"`
	Actions    []string `json:"actions"`
	Token      string   `json:"token"`
	// Key names the element across snapshots: tokens expire, names do not.
	Key string `json:"-"`
}

// Name is what a screen reader would announce.
func (e AXElement) Name() string {
	if e.Label != "" {
		return e.Label
	}
	return e.Title
}

// Can reports whether the element advertises a native action.
func (e AXElement) Can(action string) bool {
	for _, name := range e.Actions {
		if name == action {
			return true
		}
	}
	return false
}

// Disabled reports an element the app has explicitly disabled.
func (e AXElement) Disabled() bool { return e.Enabled != nil && !*e.Enabled }

// AXSnapshot is one read of a subject's native windows.
type AXSnapshot struct {
	Elements []AXElement `json:"elements"`
	Complete bool        `json:"complete"`
	Reasons  []string    `json:"incompleteReasons"`
	// Window is the main window's frame, and Surface its session token.
	Window  AXFrame `json:"-"`
	Surface string  `json:"-"`
}

// AXReading is one read taken while a window settles after an action.
type AXReading struct {
	Snapshot AXSnapshot
	At       time.Duration // since the action
	Took     time.Duration // accessibility answers on the main thread, so this tracks lag
}

// AssignKeys gives every element a name that survives a new snapshot. Equal
// names are told apart by their order, which is stable while the screen is.
func (s *AXSnapshot) AssignKeys() {
	seen := map[string]int{}
	for index := range s.Elements {
		element := &s.Elements[index]
		name := element.Name()
		if name == "" {
			name = element.Identifier
		}
		base := element.Role + "|" + name
		element.Key = fmt.Sprintf("%s#%d", base, seen[base])
		seen[base]++
	}
}

// Find returns the element with a key from an earlier snapshot.
func (s AXSnapshot) Find(key string) (AXElement, bool) {
	for _, element := range s.Elements {
		if element.Key == key {
			return element, true
		}
	}
	return AXElement{}, false
}

// Selected reports an element that is selected or sits inside something that is.
func (s AXSnapshot) Selected(element AXElement) bool {
	for {
		if element.Selected != nil && *element.Selected {
			return true
		}
		if element.Parent == nil || *element.Parent >= element.Index {
			return false
		}
		element = s.Elements[*element.Parent]
	}
}

// Signature changes when what the user can see or reach changes.
func (s AXSnapshot) Signature() string {
	digest := sha256.New()
	for _, element := range s.Elements {
		if element.Frame == nil || element.Frame.W <= 0 || element.Frame.H <= 0 {
			continue
		}
		fmt.Fprintf(digest, "%s|%s|%s|%s|%t|%t|%.0f,%.0f,%.0f,%.0f\n",
			element.Role, element.Name(), element.Identifier, element.Value,
			element.Disabled(), element.Selected != nil && *element.Selected,
			element.Frame.X, element.Frame.Y, element.Frame.W, element.Frame.H)
	}
	return hex.EncodeToString(digest.Sum(nil))[:16]
}

// Region converts a desktop rectangle to pixels of a window capture.
func (s AXSnapshot) Region(frame AXFrame, scale float64) Region {
	return Region{
		X0: int(math.Round((frame.X - s.Window.X) * scale)),
		Y0: int(math.Round((frame.Y - s.Window.Y) * scale)),
		X1: int(math.Round((frame.X+frame.W-s.Window.X)*scale)) - 1,
		Y1: int(math.Round((frame.Y+frame.H-s.Window.Y)*scale)) - 1,
	}
}

// Describe is a short phrase naming an element in a report.
func (e AXElement) Describe() string {
	role := strings.TrimPrefix(e.Role, "AX")
	if name := e.Name(); name != "" {
		return fmt.Sprintf("%s %q", role, name)
	}
	if e.Identifier != "" {
		return fmt.Sprintf("%s %s", role, e.Identifier)
	}
	return "unnamed " + role
}

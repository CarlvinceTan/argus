// Drift sensors: what changed on a screen since someone approved it.
//
// The other checks ask whether a screen is broken. A redesign nobody asked for is
// well formed, so they pass it. Comparing each step with its approved state is what
// catches that, whatever the new UI looks like.
package sensors

import (
	"encoding/json"
	"fmt"
	"image"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/carlvincetan/argus/internal/screen"
)

// Drift check identifiers.
const (
	DriftAdded  = "structure_drift_added"
	DriftGone   = "structure_drift_gone"
	DriftMoved  = "structure_drift_moved"
	DriftPixels = "structure_drift_pixels"
)

const (
	driftTolerance = 3     // points of movement that rounding or padding can explain
	driftNameMax   = 120   // long text only needs to be recognised, not stored
	pixelTolerance = 24    // per channel, so that antialiasing doesn't count
	pixelTile      = 16    // changes are counted per tile, to ignore stray pixels
	pixelTileMin   = 8     // pixels that must differ before a tile counts
	pixelShareMax  = 0.002 // share of the window, above the repeat-run noise floor
)

var (
	digits = regexp.MustCompile(`\d+`)
	// Instance numbers, clocks and counts differ on every run.
	driftVolatile = map[string]bool{"app.status": true, "connection.status": true}
	driftLayout   = map[string]bool{"AXWindow": true, "AXCell": true, "AXRow": true, "AXColumn": true, "AXGroup": true}
)

// StateRow is one element as a baseline remembers it. It serialises to a single
// line, so that a changed baseline reads as a plain diff in review.
type StateRow struct {
	Role     string
	Name     string
	X, Y     int
	W, H     int
	Scrolled bool // inside a scroll area, where vertical position depends on the scroll
}

// MarshalJSON writes the row as one string.
func (r StateRow) MarshalJSON() ([]byte, error) {
	text := fmt.Sprintf("%s|%s|%d,%d,%d,%d", r.Role, r.Name, r.X, r.Y, r.W, r.H)
	if r.Scrolled {
		text += "|scrolled"
	}
	return json.Marshal(text)
}

// UnmarshalJSON reads the one-string form. Only the name can hold a bar.
func (r *StateRow) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	text, r.Scrolled = strings.CutSuffix(text, "|scrolled")
	first := strings.Index(text, "|")
	last := strings.LastIndex(text, "|")
	if first < 0 || last <= first {
		return fmt.Errorf("baseline row %q has no name and frame", text)
	}
	r.Role, r.Name = text[:first], text[first+1:last]
	parts := strings.Split(text[last+1:], ",")
	if len(parts) != 4 {
		return fmt.Errorf("baseline row %q has no frame", text)
	}
	values := [4]int{}
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return fmt.Errorf("baseline row %q: %w", text, err)
		}
		values[index] = value
	}
	r.X, r.Y, r.W, r.H = values[0], values[1], values[2], values[3]
	return nil
}

func (r StateRow) describe() string {
	role := strings.TrimPrefix(r.Role, "AX")
	if r.Name == "" {
		return "unnamed " + role
	}
	return fmt.Sprintf("%s %q", role, r.Name)
}

func (r StateRow) region(scale float64) screen.Region {
	return screen.Region{
		X0: int(math.Round(float64(r.X) * scale)), Y0: int(math.Round(float64(r.Y) * scale)),
		X1: int(math.Round(float64(r.X+r.W)*scale)) - 1, Y1: int(math.Round(float64(r.Y+r.H)*scale)) - 1,
	}
}

// State is what a baseline remembers about one window.
type State struct {
	W    int        `json:"w"`
	H    int        `json:"h"`
	Rows []StateRow `json:"rows"`
}

// StateOf reduces a snapshot to what a person would notice changing: named things
// and controls, not the layout containers around them.
func StateOf(snapshot screen.AXSnapshot) State {
	tree := axTree{snapshot: snapshot}
	state := State{W: int(math.Round(snapshot.Window.W)), H: int(math.Round(snapshot.Window.H)), Rows: []StateRow{}}
	for _, element := range snapshot.Elements {
		name := element.Name()
		if !tree.visible(element) || driftVolatile[element.Identifier] || driftLayout[element.Role] ||
			(name == "" && !axControls[element.Role]) {
			continue
		}
		name = digits.ReplaceAllString(strings.Join(strings.Fields(name), " "), "#")
		if runes := []rune(name); len(runes) > driftNameMax {
			name = string(runes[:driftNameMax])
		}
		state.Rows = append(state.Rows, StateRow{
			Role: element.Role, Name: name,
			X: int(math.Round(element.Frame.X - snapshot.Window.X)), Y: int(math.Round(element.Frame.Y - snapshot.Window.Y)),
			W: int(math.Round(element.Frame.W)), H: int(math.Round(element.Frame.H)),
			Scrolled: tree.scrolls(element),
		})
	}
	return state
}

func driftEvidence(check string, row StateRow, detail string, scale float64) Evidence {
	return Evidence{Check: check, Region: row.region(scale), Detail: detail, Snippet: row.describe(), Severity: 2, Confidence: 0.9}
}

// Drift lists what differs from an approved state. Elements pair up by role and
// name in order, so a renamed control reads as one gone and one added.
func Drift(want, got State, scale float64, step string) []Evidence {
	found := []Evidence{}
	if want.W != got.W || want.H != got.H {
		found = append(found, Evidence{
			Check: DriftMoved, Snippet: "window size", Severity: 2, Confidence: 0.9,
			Detail: fmt.Sprintf("after %s the window is %dx%d, and the baseline was %dx%d", step, got.W, got.H, want.W, want.H),
		})
	}
	key := func(row StateRow) string { return row.Role + "|" + row.Name }
	waiting := map[string][]StateRow{}
	for _, row := range want.Rows {
		waiting[key(row)] = append(waiting[key(row)], row)
	}
	sameWindow := want.W == got.W && want.H == got.H
	for _, row := range got.Rows {
		queue := waiting[key(row)]
		if len(queue) == 0 {
			found = append(found, driftEvidence(DriftAdded, row,
				fmt.Sprintf("after %s, %s is on screen and wasn't in the baseline", step, row.describe()), scale))
			continue
		}
		old := queue[0]
		waiting[key(row)] = queue[1:]
		if !sameWindow {
			continue
		}
		// Scrolling changes where a row sits vertically, and nothing else about it.
		shifted := !row.Scrolled && !old.Scrolled && abs(row.Y-old.Y) > driftTolerance
		if shifted || abs(row.X-old.X) > driftTolerance ||
			abs(row.W-old.W) > driftTolerance || abs(row.H-old.H) > driftTolerance {
			found = append(found, driftEvidence(DriftMoved, row,
				fmt.Sprintf("after %s, %s is at %d,%d sized %dx%d, and the baseline had %d,%d sized %dx%d",
					step, row.describe(), row.X, row.Y, row.W, row.H, old.X, old.Y, old.W, old.H), scale))
		}
	}
	for _, row := range want.Rows {
		for _, missing := range waiting[key(row)] {
			if missing == row {
				found = append(found, driftEvidence(DriftGone, row,
					fmt.Sprintf("after %s, %s from the baseline is gone", step, row.describe()), scale))
			}
		}
	}
	return found
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// MissingStep reports a baseline step that can no longer be taken.
func MissingStep(action string, step int) Evidence {
	return Evidence{
		Check: DriftGone, Snippet: "step: " + action, Severity: 3, Confidence: 0.95,
		Detail: fmt.Sprintf("baseline step %d, %s, can no longer be taken", step, action),
	}
}

// Masks lists the regions pixels shouldn't be compared in: text holding a number,
// because clocks move, and whatever else the caller knows varies.
func Masks(state State, scale float64, varies func(string) bool) []screen.Region {
	masks := []screen.Region{}
	for _, row := range state.Rows {
		if strings.Contains(row.Name, "#") || (varies != nil && varies(row.describe())) {
			masks = append(masks, row.region(scale))
		}
	}
	return masks
}

// PixelDrift compares two captures of one window and returns the changed share and
// its bounds.
func PixelDrift(want, got *image.RGBA, masks []screen.Region) (float64, screen.Region) {
	bounds := got.Bounds()
	if want.Bounds() != bounds {
		return 0, screen.Region{}
	}
	masked := func(x, y int) bool {
		for _, region := range masks {
			if x >= region.X0 && x <= region.X1 && y >= region.Y0 && y <= region.Y1 {
				return true
			}
		}
		return false
	}
	near := func(a, b uint8) bool { return abs(int(a)-int(b)) <= pixelTolerance }
	changed, total := 0, 0
	area := screen.Region{X0: bounds.Max.X, Y0: bounds.Max.Y, X1: -1, Y1: -1}
	for top := bounds.Min.Y; top < bounds.Max.Y; top += pixelTile {
		for left := bounds.Min.X; left < bounds.Max.X; left += pixelTile {
			differing := 0
			for y := top; y < min(top+pixelTile, bounds.Max.Y); y++ {
				for x := left; x < min(left+pixelTile, bounds.Max.X); x++ {
					total++
					a, b := want.RGBAAt(x, y), got.RGBAAt(x, y)
					if (!near(a.R, b.R) || !near(a.G, b.G) || !near(a.B, b.B)) && !masked(x, y) {
						differing++
					}
				}
			}
			if differing < pixelTileMin {
				continue
			}
			changed += differing
			area.X0, area.Y0 = min(area.X0, left), min(area.Y0, top)
			area.X1, area.Y1 = max(area.X1, left+pixelTile-1), max(area.Y1, top+pixelTile-1)
		}
	}
	if changed == 0 || total == 0 {
		return 0, screen.Region{}
	}
	return float64(changed) / float64(total), area
}

// PixelEvidence reports a pixel change large enough to matter, or nothing.
func PixelEvidence(share float64, area screen.Region, step string) []Evidence {
	if share <= pixelShareMax {
		return nil
	}
	return []Evidence{{
		Check: DriftPixels, Region: area, Snippet: "pixels after " + step, Severity: 2, Confidence: 0.8, Measured: share,
		Detail: fmt.Sprintf("after %s, %.2f%% of the window differs from the baseline image, with the same controls", step, share*100),
	}}
}

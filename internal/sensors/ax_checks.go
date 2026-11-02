// Accessibility sensors: deterministic checks for native windows.
//
// Native apps give no cell grid, but their accessibility tree states where every
// control is. Everything here is measured from that tree, so a run needs no model
// to report it. Text inside opaque views is absent from the tree; clipped or
// overlapping prose stays with the vision tier.
package sensors

import (
	"fmt"
	"image"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/carlvincetan/argus/internal/screen"
)

// Native check identifiers. The structure prefix marks them as measured facts,
// which a model may weigh but cannot silently discard.
const (
	AXUnlabelled = "structure_ax_unlabelled"
	AXOverlap    = "structure_ax_overlap"
	AXClipped    = "structure_ax_clipped"
	AXSmallHit   = "structure_ax_small_target"
	AXCutText    = "structure_ax_cut_text"
	AXBlank      = "structure_ax_blank"
	AXInert      = "structure_ax_inert"
	AXCrash      = "structure_ax_crash"
	AXFlash      = "structure_ax_flash"
	AXShift      = "structure_ax_shift"
	LogWarning   = "structure_log_warning"
)

const (
	axMinVisible    = 4.0                    // smaller elements are layout probes, not UI
	axSmallTarget   = 16.0                   // an app can widen a hit area past its frame, so this stays a hint
	axClipTolerance = 2.0                    // points of overhang that rounding can explain
	axOverlapMin    = 0.15                   // share of the smaller element that must collide
	axContainMax    = 0.9                    // above this, one control sits inside the other on purpose
	axCutMaxChars   = 40                     // long labels truncate by design
	axCutMinChars   = 4                      // a separator dot or a count says nothing about clipping
	axCutMaxLine    = 24.0                   // taller text wraps, so width says nothing
	axCharWidth     = 0.5                    // average glyph width as a share of font size
	axLineHeight    = 1.2                    // line height as a share of font size
	axCutMargin     = 1.25                   // proportional fonts vary, so require a clear excess
	axFlashMax      = 400 * time.Millisecond // longer than this reads as real loading
	axPaneShare     = 0.7                    // a pane covers less than this share of the window
	axShiftMin      = 4.0                    // points; less is rounding or a hover effect
	blankShare      = 0.001                  // a thin logo is about this much of a window
	blankTolerance  = 12                     // per channel, so that gradients count as one colour
)

var axControls = map[string]bool{
	"AXButton": true, "AXCheckBox": true, "AXLink": true, "AXMenuButton": true,
	"AXPopUpButton": true, "AXRadioButton": true, "AXSlider": true, "AXSwitch": true,
	"AXTextArea": true, "AXTextField": true,
}

var axChrome = map[string]bool{
	"AXCloseButton": true, "AXFullScreenButton": true, "AXMinimizeButton": true, "AXZoomButton": true,
}

// IsWindowChrome reports the traffic-light buttons, which belong to the system.
func IsWindowChrome(subrole string) bool { return axChrome[subrole] }

// axTree indexes a snapshot for parent lookups.
type axTree struct {
	snapshot screen.AXSnapshot
	scale    float64
}

func (t axTree) parent(element screen.AXElement) (screen.AXElement, bool) {
	if element.Parent == nil || *element.Parent < 0 || *element.Parent >= len(t.snapshot.Elements) {
		return screen.AXElement{}, false
	}
	return t.snapshot.Elements[*element.Parent], true
}

func (t axTree) scrolls(element screen.AXElement) bool {
	for current, ok := t.parent(element); ok; current, ok = t.parent(current) {
		if current.Role == "AXScrollArea" {
			return true
		}
	}
	return false
}

func (t axTree) ancestor(element, other screen.AXElement) bool {
	for current, ok := t.parent(element); ok; current, ok = t.parent(current) {
		if current.Index == other.Index {
			return true
		}
	}
	return false
}

func (t axTree) visible(element screen.AXElement) bool {
	return element.Frame != nil && element.Frame.W >= axMinVisible && element.Frame.H >= axMinVisible &&
		!axChrome[element.Subrole]
}

func (t axTree) evidence(check string, element screen.AXElement, detail string, severity, confidence float64) Evidence {
	return Evidence{
		Check:      check,
		Region:     t.snapshot.Region(*element.Frame, t.scale),
		Detail:     detail,
		Snippet:    element.Describe(),
		Severity:   severity,
		Confidence: confidence,
	}
}

// RunAXChecks measures one snapshot. An incomplete read proves nothing about
// what is missing, so it reports nothing.
func RunAXChecks(snapshot screen.AXSnapshot, scale float64) []Evidence {
	if !snapshot.Complete {
		return nil
	}
	tree := axTree{snapshot: snapshot, scale: scale}
	controls := []screen.AXElement{}
	found := []Evidence{}
	content := 0
	for _, element := range snapshot.Elements {
		if !tree.visible(element) {
			continue
		}
		if element.Role == "AXStaticText" && element.Identifier != "app.status" {
			content++
			if item, ok := tree.cutText(element); ok {
				found = append(found, item)
			}
		}
		if !axControls[element.Role] {
			continue
		}
		content++
		controls = append(controls, element)
		if element.Disabled() {
			continue
		}
		if element.Name() == "" {
			found = append(found, tree.evidence(AXUnlabelled, element,
				fmt.Sprintf("%s has no accessible name, so a screen reader announces nothing", element.Describe()), 2, 0.9))
		}
		if item, ok := tree.clipped(element); ok {
			found = append(found, item)
		}
		if element.Can("AXPress") && (element.Frame.W < axSmallTarget || element.Frame.H < axSmallTarget) {
			found = append(found, tree.evidence(AXSmallHit, element,
				fmt.Sprintf("%s is only %.0fx%.0f points", element.Describe(), element.Frame.W, element.Frame.H), 1, 0.6))
		}
	}
	found = append(found, tree.overlaps(controls)...)
	if content == 0 && len(snapshot.Elements) > 0 {
		root := snapshot.Elements[0]
		if root.Frame != nil {
			found = append(found, tree.evidence(AXBlank, root, "the window shows no text and no controls", 3, 0.9))
		}
	}
	return found
}

// clipped reports a control hanging out of its window. Scrolled content leaves
// its viewport all the time, so only fixed layout counts.
func (t axTree) clipped(element screen.AXElement) (Evidence, bool) {
	if t.scrolls(element) {
		return Evidence{}, false
	}
	frame, window := *element.Frame, t.snapshot.Window
	over := maxFloat(window.X-frame.X, frame.X+frame.W-window.X-window.W)
	over = maxFloat(over, maxFloat(window.Y-frame.Y, frame.Y+frame.H-window.Y-window.H))
	if over <= axClipTolerance {
		return Evidence{}, false
	}
	item := t.evidence(AXClipped, element,
		fmt.Sprintf("%s extends %.0f points outside the window", element.Describe(), over), 2.5, 0.9)
	item.Measured = over
	return item, true
}

// overlaps reports controls that partly cover each other. A control fully inside
// another is usually a deliberate composition, and is left alone.
func (t axTree) overlaps(controls []screen.AXElement) []Evidence {
	found := []Evidence{}
	for first := 0; first < len(controls); first++ {
		for second := first + 1; second < len(controls); second++ {
			a, b := controls[first], controls[second]
			if t.ancestor(a, b) || t.ancestor(b, a) || t.scrolls(a) != t.scrolls(b) {
				continue
			}
			share := overlapShare(*a.Frame, *b.Frame)
			if share < axOverlapMin || share > axContainMax {
				continue
			}
			item := t.evidence(AXOverlap, a,
				fmt.Sprintf("%s and %s overlap by %.0f%%", a.Describe(), b.Describe(), share*100), 2.5, 0.85)
			item.Snippet = a.Describe() + " + " + b.Describe()
			item.Measured = share
			found = append(found, item)
		}
	}
	return found
}

// overlapShare is the intersection as a share of the smaller rectangle.
func overlapShare(a, b screen.AXFrame) float64 {
	width := minFloat(a.X+a.W, b.X+b.W) - maxFloat(a.X, b.X)
	height := minFloat(a.Y+a.H, b.Y+b.H) - maxFloat(a.Y, b.Y)
	if width <= 0 || height <= 0 {
		return 0
	}
	return width * height / minFloat(a.W*a.H, b.W*b.H)
}

// cutText estimates whether a short single-line label fits its frame. The tree
// gives the full string and the frame it got, not the font, so the estimate comes
// from the line height and only a clear excess counts.
func (t axTree) cutText(element screen.AXElement) (Evidence, bool) {
	text := strings.TrimSpace(element.Value)
	if text == "" {
		text = strings.TrimSpace(element.Name())
	}
	count := utf8.RuneCountInString(text)
	if count < axCutMinChars || count > axCutMaxChars || element.Frame.H > axCutMaxLine {
		return Evidence{}, false
	}
	needed := float64(count) * axCharWidth * element.Frame.H / axLineHeight
	if needed <= element.Frame.W*axCutMargin {
		return Evidence{}, false
	}
	item := t.evidence(AXCutText, element,
		fmt.Sprintf("%s needs about %.0f points but has %.0f", element.Describe(), needed, element.Frame.W), 2, 0.6)
	item.Measured = needed
	return item, true
}

// Persisting keeps evidence that a later read still measures. A fading banner or
// a sliding panel gets measured mid-flight; only what stays is a real state.
func Persisting(found, later []Evidence) []Evidence {
	still := map[string]bool{}
	for _, item := range later {
		still[EvidenceKey(item)] = true
	}
	out := []Evidence{}
	for _, item := range found {
		if still[EvidenceKey(item)] {
			out = append(out, item)
		}
	}
	return out
}

// EvidenceKey identifies a measurement independent of where the element sits.
func EvidenceKey(item Evidence) string { return item.Check + "|" + item.Snippet }

// ShowsContent reports whether an image holds more than one flat colour, because
// a splash or a canvas can fill a window that exposes nothing to accessibility.
func ShowsContent(img *image.RGBA) bool {
	bounds := img.Bounds()
	if bounds.Empty() {
		return false
	}
	base := img.RGBAAt(bounds.Min.X+bounds.Dx()/2, bounds.Min.Y+bounds.Dy()/8)
	differ := func(a, b uint8) bool { return int(a)-int(b) > blankTolerance || int(b)-int(a) > blankTolerance }
	other, total := 0, 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y += 2 {
		for x := bounds.Min.X; x < bounds.Max.X; x += 2 {
			pixel := img.RGBAAt(x, y)
			total++
			if differ(pixel.R, base.R) || differ(pixel.G, base.G) || differ(pixel.B, base.B) {
				other++
			}
		}
	}
	return float64(other)/float64(total) >= blankShare
}

func axTextKey(element screen.AXElement) string {
	return element.Role + "|" + element.Name() + "|" + element.Value
}

// Flashes reports text or a spinner that came and went while a window settled, the
// empty state or loading line that a warm cache would never have shown.
func Flashes(before screen.AXSnapshot, readings []screen.AXReading, scale float64) []Evidence {
	if len(readings) < 2 {
		return nil
	}
	settled := map[string]bool{}
	for _, element := range before.Elements {
		settled[axTextKey(element)] = true
	}
	final := readings[len(readings)-1]
	for _, element := range final.Snapshot.Elements {
		settled[axTextKey(element)] = true
	}
	first := map[string]time.Duration{}
	last := map[string]time.Duration{}
	found := map[string]screen.AXElement{}
	sources := map[string]screen.AXSnapshot{}
	for index, reading := range readings[:len(readings)-1] {
		for _, element := range reading.Snapshot.Elements {
			key := axTextKey(element)
			tree := axTree{snapshot: reading.Snapshot, scale: scale}
			if settled[key] || !tree.visible(element) || (element.Role != "AXStaticText" && element.Role != "AXProgressIndicator" && element.Role != "AXBusyIndicator") {
				continue
			}
			if _, seen := first[key]; !seen {
				first[key], found[key], sources[key] = reading.At, element, reading.Snapshot
			}
			last[key] = readings[index+1].At
		}
	}
	out := []Evidence{}
	for key, element := range found {
		shown := last[key] - first[key]
		if shown > axFlashMax {
			continue
		}
		tree := axTree{snapshot: sources[key], scale: scale}
		item := tree.evidence(AXFlash, element,
			fmt.Sprintf("%s appeared for about %d ms and was gone", element.Describe(), shown.Milliseconds()), 2, 0.75)
		item.Measured = float64(shown.Milliseconds())
		out = append(out, item)
	}
	return out
}

// Shifts reports controls outside the pressed element's pane that moved: pressing
// something should touch only what it changes.
func Shifts(before, after screen.AXSnapshot, pressed screen.AXElement, scale float64) []Evidence {
	pane, ok := axPane(before, pressed)
	if !ok {
		return nil
	}
	tree := axTree{snapshot: after, scale: scale}
	found := []Evidence{}
	for _, element := range before.Elements {
		if !tree.visible(element) || !axControls[element.Role] {
			continue
		}
		moved, ok := after.Find(element.Key)
		if !ok || !tree.visible(moved) || inside(*element.Frame, pane) {
			continue
		}
		distance := math.Abs(moved.Frame.X-element.Frame.X) + math.Abs(moved.Frame.Y-element.Frame.Y)
		if distance <= axShiftMin {
			continue
		}
		item := tree.evidence(AXShift, moved,
			fmt.Sprintf("pressing %s moved %s by %.0f points", pressed.Describe(), moved.Describe(), distance), 1.2, 0.65)
		item.Measured = distance
		found = append(found, item)
	}
	return found
}

// axPane finds the largest ancestor that is still only part of the window.
func axPane(snapshot screen.AXSnapshot, element screen.AXElement) (screen.AXFrame, bool) {
	area := snapshot.Window.W * snapshot.Window.H
	best, ok := screen.AXFrame{}, false
	for element.Parent != nil && *element.Parent < element.Index {
		element = snapshot.Elements[*element.Parent]
		if element.Frame != nil && element.Frame.W*element.Frame.H < area*axPaneShare && element.Frame.W*element.Frame.H > best.W*best.H {
			best, ok = *element.Frame, true
		}
	}
	return best, ok
}

func inside(frame, pane screen.AXFrame) bool {
	return frame.X >= pane.X && frame.Y >= pane.Y && frame.X+frame.W <= pane.X+pane.W && frame.Y+frame.H <= pane.Y+pane.H
}

// InertControl records a press that changed nothing a user can see.
func InertControl(snapshot screen.AXSnapshot, element screen.AXElement, scale float64) Evidence {
	tree := axTree{snapshot: snapshot, scale: scale}
	return tree.evidence(AXInert, element,
		fmt.Sprintf("pressing %s changed nothing on screen or in the accessibility tree", element.Describe()), 2, 0.7)
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

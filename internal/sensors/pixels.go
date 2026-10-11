// Pixel sensors: the GUI counterpart of the terminal checks.
//
// A GUI subject gives no cell grid, so the deterministic tier works on frames
// instead: changed-pixel ratios, per-block flicker, full repaints, layout drift
// and idle churn. Everything here is measured on pixels over time, which is
// exactly what a user perceives, and each finding carries the pixel region a
// reviewer can see highlighted in the evidence image.
//
// What pixels cannot decide (a clipped word, overlapping text, low contrast) is
// judged by the vision tier and the decision tier; the split is deliberate and
// documented rather than pretended.
package sensors

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"time"

	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/screen"
)

// GUI check identifiers.
const (
	GUIFlicker     = "gui_flicker"
	GUIFullRepaint = "gui_full_repaint"
	GUILayoutDrift = "gui_layout_drift"
	GUIIdleChurn   = "gui_idle_churn"
)

const (
	guiBlockSize      = 16   // flicker is measured on 16x16 tiles
	guiFlickerCycles  = 2    // A -> B -> A transitions before it is called flicker
	guiDriftPixels    = 3    // a content block moving this far while idle is drift
	guiFullRepaintPct = 0.55 // share of pixels that must change for a full repaint
)

// MotionReportGUI mirrors MotionReport for pixel frames.
type MotionReportGUI struct {
	Frames               int
	DurationMS           int64
	ChangedPixelsPerSec  float64
	IdleChangedPixelRate float64
	Evidence             []Evidence
}

// AnalyzePixels inspects a frame timeline for animation-level defects.
func AnalyzePixels(
	frames []*screen.Frame,
	inputs []screen.InputEvent,
	thresholds config.Thresholds,
) MotionReportGUI {
	report := MotionReportGUI{Frames: len(frames)}
	if len(frames) < 2 {
		return report
	}
	report.DurationMS = (frames[len(frames)-1].T - frames[0].T).Milliseconds()
	total := 0
	for _, frame := range frames {
		total += frame.ChangedPixels
	}
	if report.DurationMS > 0 {
		report.ChangedPixelsPerSec = float64(total) / (float64(report.DurationMS) / 1000)
	}
	report.Evidence = append(report.Evidence, pixelIdleChurn(frames, inputs, thresholds)...)
	report.Evidence = append(report.Evidence, pixelFlicker(frames)...)
	report.Evidence = append(report.Evidence, pixelFullRepaint(frames)...)
	report.Evidence = append(report.Evidence, pixelDrift(frames, inputs)...)
	for _, item := range report.Evidence {
		if item.Check == GUIIdleChurn && item.Measured > report.IdleChangedPixelRate {
			report.IdleChangedPixelRate = item.Measured
		}
	}
	return report
}

// pixelIdleChurn reports repainting while the user is not doing anything.
func pixelIdleChurn(frames []*screen.Frame, inputs []screen.InputEvent, thresholds config.Thresholds) []Evidence {
	out := []Evidence{}
	if len(frames) < 4 {
		return out
	}
	start := 0
	for index := 1; index <= len(frames); index++ {
		boundary := index == len(frames)
		if !boundary {
			if input, ok := inputBefore(inputs, frames[index].T); ok &&
				frames[index].T-input.T < 500*time.Millisecond {
				boundary = true
			}
		}
		if !boundary {
			continue
		}
		if index-start >= 3 {
			changed := 0
			for cursor := start + 1; cursor < index; cursor++ {
				changed += frames[cursor].ChangedPixels
			}
			elapsed := (frames[index-1].T - frames[start].T).Seconds()
			if elapsed > 0 {
				pixels := float64(frames[start].TotalPixels)
				rate := float64(changed) / elapsed / math.Max(pixels, 1)
				if rate > thresholds.IdleChurn {
					bounds := changedBounds(frames[start].Image, frames[index-1].Image)
					out = append(out, Evidence{
						Check:      GUIIdleChurn,
						Region:     bounds,
						Detail:     fmt.Sprintf("the window repaints while idle: %.1f%% of pixels change per second with no input for %.1fs", rate*100, elapsed),
						Severity:   2.0,
						Confidence: 0.75,
						Measured:   rate,
						GridID:     start,
					})
					start = index
					continue
				}
			}
		}
		start = index
	}
	return out
}

// pixelFlicker finds tiles whose appearance alternates back and forth.
//
// It follows each tile's appearance over the whole window rather than assuming a
// fixed frame distance, because a blink can be slower or faster than the sampling
// interval and the detector must not depend on that.
func pixelFlicker(frames []*screen.Frame) []Evidence {
	if len(frames) < 4 {
		return nil
	}
	type tile struct{ x, y int }
	signatures := map[tile][]uint32{}
	for _, frame := range frames {
		if frame.Image == nil {
			return nil
		}
		bounds := frame.Image.Bounds()
		for y := bounds.Min.Y; y+guiBlockSize <= bounds.Max.Y; y += guiBlockSize {
			for x := bounds.Min.X; x+guiBlockSize <= bounds.Max.X; x += guiBlockSize {
				signatures[tile{x, y}] = append(signatures[tile{x, y}], tileSignature(frame.Image, x, y))
			}
		}
	}
	if len(signatures) == 0 {
		return nil
	}
	flashing := map[tile]int{}
	for position, series := range signatures {
		compressed := []uint32{}
		for _, value := range series {
			if len(compressed) == 0 || compressed[len(compressed)-1] != value {
				compressed = append(compressed, value)
			}
		}
		if len(compressed) < 3 {
			continue
		}
		alternations := 0
		for index := 2; index < len(compressed); index++ {
			if compressed[index] == compressed[index-2] && compressed[index] != compressed[index-1] {
				alternations++
			}
		}
		if alternations >= guiFlickerCycles {
			flashing[position] = alternations
		}
	}
	if len(flashing) == 0 {
		return nil
	}
	region := screen.Region{}
	count, transitions := 0, 0
	for position, value := range flashing {
		if count == 0 {
			region = screen.Region{X0: position.x, Y0: position.y, X1: position.x + guiBlockSize - 1, Y1: position.y + guiBlockSize - 1}
		} else {
			region.X0 = minInt(region.X0, position.x)
			region.Y0 = minInt(region.Y0, position.y)
			region.X1 = maxInt(region.X1, position.x+guiBlockSize-1)
			region.Y1 = maxInt(region.Y1, position.y+guiBlockSize-1)
		}
		count++
		transitions += value
	}
	return []Evidence{{
		Check:      GUIFlicker,
		Region:     region,
		Detail:     fmt.Sprintf("%d tile(s) alternate back and forth between frames (%d transitions over %d frames), which reads as flicker", count, transitions, len(frames)),
		Severity:   2.5,
		Confidence: 0.8,
		Measured:   float64(count),
		GridID:     len(frames) - 1,
	}}
}

// tileSignature reduces a tile to a coarse identity so identical states compare
// equal even when antialiasing shifts a pixel or two.
func tileSignature(img *image.RGBA, x, y int) uint32 {
	step := guiBlockSize / 4
	if step < 1 {
		step = 1
	}
	var sum uint32
	for row := y; row < y+guiBlockSize; row += step {
		for column := x; column < x+guiBlockSize; column += step {
			colour := img.RGBAAt(column, row)
			sum = sum*31 + uint32(colour.R/32)<<12 + uint32(colour.G/32)<<6 + uint32(colour.B/32)
		}
	}
	return sum
}

// pixelFullRepaint finds naive redraw loops: almost every pixel changes at once.
func pixelFullRepaint(frames []*screen.Frame) []Evidence {
	out := []Evidence{}
	for index := 1; index < len(frames); index++ {
		frame := frames[index]
		if frame.TotalPixels == 0 {
			continue
		}
		ratio := float64(frame.ChangedPixels) / float64(frame.TotalPixels)
		if ratio < guiFullRepaintPct {
			continue
		}
		deltaMS := (frame.T - frames[index-1].T).Milliseconds()
		out = append(out, Evidence{
			Check:      GUIFullRepaint,
			Region:     screen.Region{X0: 0, Y0: 0, X1: frame.Width - 1, Y1: frame.Height - 1},
			Detail:     fmt.Sprintf("%.0f%% of the window is repainted in a single frame (%dms after the previous one)", ratio*100, deltaMS),
			Severity:   2.0,
			Confidence: 0.6,
			Measured:   ratio,
			GridID:     index,
		})
		if len(out) >= 3 {
			break
		}
	}
	return out
}

// pixelDrift reports a content block that moves while the window is idle.
func pixelDrift(frames []*screen.Frame, inputs []screen.InputEvent) []Evidence {
	out := []Evidence{}
	previousBounds, previousGrid := image.Rectangle{}, -1
	for index, frame := range frames {
		if frame.Image == nil {
			continue
		}
		if input, ok := inputBefore(inputs, frame.T); ok && frame.T-input.T < 500*time.Millisecond {
			previousGrid = index
			continue
		}
		bounds := contentBounds(frame.Image)
		if previousGrid >= 0 && previousBounds != (image.Rectangle{}) && bounds != (image.Rectangle{}) {
			dx, dy := bounds.Min.X-previousBounds.Min.X, bounds.Min.Y-previousBounds.Min.Y
			if absInt(dx) >= guiDriftPixels || absInt(dy) >= guiDriftPixels {
				out = append(out, Evidence{
					Check:      GUILayoutDrift,
					Region:     screen.Region{X0: bounds.Min.X, Y0: bounds.Min.Y, X1: bounds.Max.X - 1, Y1: bounds.Max.Y - 1},
					Detail:     fmt.Sprintf("the content block moves by (%d,%d) pixels while the window is idle", dx, dy),
					Severity:   1.5,
					Confidence: 0.5,
					Measured:   float64(maxInt(absInt(dx), absInt(dy))),
					GridID:     index,
				})
				if len(out) >= 3 {
					return out
				}
			}
		}
		previousBounds, previousGrid = bounds, index
	}
	return out
}

// contentBounds is the bounding box of pixels that differ from the window's most
// common colour (the background), i.e. where the content is.
func contentBounds(img *image.RGBA) image.Rectangle {
	if img == nil {
		return image.Rectangle{}
	}
	background := dominantColour(img)
	minX, minY := img.Bounds().Max.X, img.Bounds().Max.Y
	maxX, maxY := img.Bounds().Min.X, img.Bounds().Min.Y
	found := false
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y += 2 {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x += 2 {
			colour := img.RGBAAt(x, y)
			if absInt(int(colour.R)-int(background.R)) < 12 &&
				absInt(int(colour.G)-int(background.G)) < 12 &&
				absInt(int(colour.B)-int(background.B)) < 12 {
				continue
			}
			found = true
			minX, minY = minInt(minX, x), minInt(minY, y)
			maxX, maxY = maxInt(maxX, x), maxInt(maxY, y)
		}
	}
	if !found {
		return image.Rectangle{}
	}
	return image.Rect(minX, minY, maxX+1, maxY+1)
}

// changedBounds is the bounding box of the pixels that changed between frames.
func changedBounds(a, b *image.RGBA) screen.Region {
	if a == nil || b == nil || a.Bounds() != b.Bounds() {
		return screen.Region{X0: 0, Y0: 0, X1: 0, Y1: 0}
	}
	minX, minY := a.Bounds().Max.X, a.Bounds().Max.Y
	maxX, maxY := a.Bounds().Min.X, a.Bounds().Min.Y
	found := false
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			if a.RGBAAt(x, y) == b.RGBAAt(x, y) {
				continue
			}
			found = true
			minX, minY = minInt(minX, x), minInt(minY, y)
			maxX, maxY = maxInt(maxX, x), maxInt(maxY, y)
		}
	}
	if !found {
		return screen.Region{X0: 0, Y0: 0, X1: 0, Y1: 0}
	}
	return screen.Region{X0: minX, Y0: minY, X1: maxX, Y1: maxY}
}

func dominantColour(img *image.RGBA) color.RGBA {
	counts := map[uint32]int{}
	best, bestCount := img.RGBAAt(img.Bounds().Min.X, img.Bounds().Min.Y), 0
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y += 3 {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x += 3 {
			colour := img.RGBAAt(x, y)
			key := uint32(colour.R)<<16 | uint32(colour.G)<<8 | uint32(colour.B)
			counts[key]++
			if counts[key] > bestCount {
				best, bestCount = colour, counts[key]
			}
		}
	}
	return best
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

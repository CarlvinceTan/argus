package sensors

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/screen"
)

// Motion check identifiers.
const (
	CheckFlicker       = "flicker"
	CheckFullClear     = "full_screen_clear"
	CheckFooterDrift   = "layout_drift"
	CheckStuckSpinner  = "stuck_spinner"
	CheckCadenceJitter = "animation_cadence"
	CheckIdleChurn     = "idle_redraw_churn"
)

const (
	idleSettle           = 500 * time.Millisecond
	flickerWindow        = 1500 * time.Millisecond
	minFlickerCycles     = 2
	cadenceBurstMax      = 200 * time.Millisecond
	minCadenceIntervals  = 4
	cadenceJitterLimit   = 0.8
	loadingTextLimit     = 120
	stuckSpinnerMinIdleS = 1.0
)

// MotionReport summarises how the screen behaved over time.
type MotionReport struct {
	Grids                int           `json:"grids"`
	Frames               int           `json:"frames"`
	Duration             time.Duration `json:"duration"`
	ChangeRatePerSec     float64       `json:"change_rate_per_sec"`
	IdleChangeRatePerSec float64       `json:"idle_change_rate_per_sec"`
	IdleWindows          int           `json:"idle_windows"`
	LastInputAge         time.Duration `json:"last_input_age"`
	IdleStill            bool          `json:"idle_still"`
	Evidence             []Evidence    `json:"evidence"`
}

// AnalyzeMotion inspects the frame timeline for animation-level defects.
func AnalyzeMotion(
	grids []*screen.Grid,
	frames []screen.FrameEvent,
	inputs []screen.InputEvent,
	thresholds config.Thresholds,
) MotionReport {
	report := MotionReport{Grids: len(grids), Frames: len(frames)}
	if len(grids) == 0 {
		return report
	}
	report.Duration = grids[len(grids)-1].Time - grids[0].Time
	if report.Duration > 0 {
		totalChanged := 0
		for _, frame := range frames {
			totalChanged += frame.ChangedCells
		}
		report.ChangeRatePerSec = float64(totalChanged) / report.Duration.Seconds()
	}
	if len(inputs) > 0 {
		report.LastInputAge = grids[len(grids)-1].Time - inputs[len(inputs)-1].T
	}

	report.Evidence = append(report.Evidence, idleChurn(grids, inputs, thresholds)...)
	report.Evidence = append(report.Evidence, flickerEvents(grids, thresholds)...)
	report.Evidence = append(report.Evidence, fullClearEvents(grids)...)
	report.Evidence = append(report.Evidence, layoutDrift(grids, inputs)...)
	report.Evidence = append(report.Evidence, stuckSpinners(grids)...)
	report.Evidence = append(report.Evidence, cadenceEvents(grids)...)
	for _, item := range report.Evidence {
		if item.Check == CheckIdleChurn {
			report.IdleWindows++
			if item.Measured > report.IdleChangeRatePerSec {
				report.IdleChangeRatePerSec = item.Measured
			}
		}
	}
	report.IdleStill = len(report.Evidence) == 0 && report.LastInputAge > idleSettle
	return report
}

// inputBefore returns the most recent input at or before a timestamp.
func inputBefore(inputs []screen.InputEvent, at time.Duration) (screen.InputEvent, bool) {
	var found screen.InputEvent
	ok := false
	for _, input := range inputs {
		if input.T <= at {
			found, ok = input, true
			continue
		}
		break
	}
	return found, ok
}

// idleChurn reports windows with no recent input during which the screen keeps
// changing: a repaint loop the user did not ask for.
func idleChurn(grids []*screen.Grid, inputs []screen.InputEvent, thresholds config.Thresholds) []Evidence {
	out := []Evidence{}
	if len(grids) < 3 {
		return out
	}
	windowStart := 0
	for index := 1; index <= len(grids); index++ {
		boundary := index == len(grids)
		if !boundary {
			if input, ok := inputBefore(inputs, grids[index].Time); ok && grids[index].Time-input.T < idleSettle {
				boundary = true
			}
		}
		if !boundary {
			continue
		}
		if index-windowStart >= 3 {
			changed := 0
			for cursor := windowStart + 1; cursor < index; cursor++ {
				changed += grids[cursor].Changed(grids[cursor-1])
			}
			elapsed := (grids[index-1].Time - grids[windowStart].Time).Seconds()
			if elapsed > 0 {
				cells := float64(grids[windowStart].Cols * grids[windowStart].RowsN)
				rate := float64(changed) / elapsed / math.Max(cells, 1)
				if rate > thresholds.IdleChurn {
					out = append(out, Evidence{
						Check:      CheckIdleChurn,
						Region:     screen.Region{X0: 0, Y0: 0, X1: grids[windowStart].Cols - 1, Y1: grids[windowStart].RowsN - 1},
						Detail:     fmt.Sprintf("screen keeps repainting while idle: %.1f%% of cells change per second with no input for %.1fs", rate*100, elapsed),
						Severity:   2.0,
						Confidence: 0.75,
						Measured:   rate,
						GridID:     grids[windowStart].ID,
					})
					windowStart = index
					continue
				}
			}
		}
		windowStart = index
	}
	return out
}

// flickerEvents finds cells that alternate back and forth (A B A) inside a short
// window, which is what a user perceives as flicker.
func flickerEvents(grids []*screen.Grid, thresholds config.Thresholds) []Evidence {
	required := thresholds.FlickerCycles
	if required < minFlickerCycles {
		required = minFlickerCycles
	}
	counts := map[[2]int]int{}
	samples := map[[2]int][]string{}
	for index := 2; index < len(grids); index++ {
		first, middle, last := grids[index-2], grids[index-1], grids[index]
		if last.Time-first.Time > flickerWindow {
			continue
		}
		if first.Cols != last.Cols || first.RowsN != last.RowsN ||
			first.Cols != middle.Cols || first.RowsN != middle.RowsN {
			continue
		}
		for y := 0; y < first.RowsN; y++ {
			if first.Rows[y] == last.Rows[y] && first.Rows[y] == middle.Rows[y] {
				continue
			}
			for x := 0; x < first.Cols; x++ {
				before, okBefore := first.At(x, y)
				during, okDuring := middle.At(x, y)
				after, okAfter := last.At(x, y)
				if !okBefore || !okDuring || !okAfter {
					continue
				}
				if before.Char == after.Char && during.Char != before.Char {
					position := [2]int{x, y}
					counts[position]++
					if len(samples[position]) < 4 {
						samples[position] = append(samples[position],
							fmt.Sprintf("%q>%q>%q", string(before.Char), string(during.Char), string(after.Char)))
					}
				}
			}
		}
	}
	if len(counts) == 0 {
		return nil
	}
	// Cluster flickering cells into contiguous regions.
	cluster := map[[2]int]int{}
	clusters := map[int][]int{}
	nextCluster := 0
	for position, count := range counts {
		if count < required {
			continue
		}
		assigned := -1
		for _, delta := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
			neighbour := [2]int{position[0] + delta[0], position[1] + delta[1]}
			if id, ok := cluster[neighbour]; ok {
				assigned = id
				break
			}
		}
		if assigned < 0 {
			assigned = nextCluster
			nextCluster++
		}
		cluster[position] = assigned
		clusters[assigned] = append(clusters[assigned], position[0], position[1])
	}
	out := []Evidence{}
	for id, coordinates := range clusters {
		if len(coordinates) < 2 {
			continue
		}
		x0, y0, x1, y1 := coordinates[0], coordinates[1], coordinates[0], coordinates[1]
		count := 0
		for index := 0; index < len(coordinates); index += 2 {
			x, y := coordinates[index], coordinates[index+1]
			x0, y0 = min(x0, x), min(y0, y)
			x1, y1 = max(x1, x), max(y1, y)
			count += counts[[2]int{x, y}]
		}
		out = append(out, Evidence{
			Check:      CheckFlicker,
			Region:     screen.Region{X0: x0, Y0: y0, X1: x1, Y1: y1},
			Detail:     fmt.Sprintf("%d cells alternate back and forth between frames (%d transitions), which reads as flicker", (len(coordinates))/2, count),
			Snippet:    sampleFor(samples, coordinates),
			Severity:   2.5,
			Confidence: 0.8,
			GridID:     grids[len(grids)-1].ID,
		})
		_ = id
		if len(out) >= 4 {
			break
		}
	}
	return out
}

func sampleFor(samples map[[2]int][]string, coordinates []int) string {
	for index := 0; index < len(coordinates); index += 2 {
		position := [2]int{coordinates[index], coordinates[index+1]}
		if values, ok := samples[position]; ok && len(values) > 0 {
			return fmt.Sprintf("cell %d,%d: %s", position[0], position[1], strings.Join(values, " "))
		}
	}
	return ""
}

// fullClearEvents finds screens that blank out and repaint, the classic
// full-screen flicker of a program that clears before drawing.
func fullClearEvents(grids []*screen.Grid) []Evidence {
	out := []Evidence{}
	for index := 1; index < len(grids); index++ {
		previous, current := grids[index-1], grids[index]
		if previous.NonSpace() < 50 {
			continue
		}
		if float64(current.NonSpace()) > 0.25*float64(previous.NonSpace()) {
			continue
		}
		restored := false
		for cursor := index + 1; cursor < len(grids) && cursor <= index+4; cursor++ {
			if grids[cursor].Time-previous.Time > flickerWindow {
				break
			}
			if float64(grids[cursor].NonSpace()) >= 0.75*float64(previous.NonSpace()) {
				restored = true
				break
			}
		}
		if !restored {
			continue
		}
		out = append(out, Evidence{
			Check:      CheckFullClear,
			Region:     screen.Region{X0: 0, Y0: 0, X1: current.Cols - 1, Y1: current.RowsN - 1},
			Detail:     fmt.Sprintf("the whole screen blanks (%d visible cells down from %d) and is redrawn %.0fms later", current.NonSpace(), previous.NonSpace(), float64(current.Time-previous.Time)/float64(time.Millisecond)),
			Severity:   3.0,
			Confidence: 0.75,
			GridID:     current.ID,
		})
		if len(out) >= 3 {
			break
		}
	}
	return out
}

// layoutDrift reports a status or footer line that moves while the screen is
// otherwise idle, which is the layout-jitter class of bug.
func layoutDrift(grids []*screen.Grid, inputs []screen.InputEvent) []Evidence {
	out := []Evidence{}
	lastRow := func(grid *screen.Grid) int {
		for y := grid.RowsN - 1; y >= 0; y-- {
			if strings.TrimSpace(grid.Row(y)) != "" {
				return y
			}
		}
		return -1
	}
	inset := func(grid *screen.Grid) int {
		for y := grid.RowsN - 1; y >= 0; y-- {
			row := grid.Row(y)
			if strings.TrimSpace(row) != "" {
				return len(row) - len(strings.TrimLeft(row, " "))
			}
		}
		return -1
	}
	_ = inset
	for index := 1; index < len(grids); index++ {
		previous, current := grids[index-1], grids[index]
		if current.Time-previous.Time > flickerWindow {
			continue
		}
		if input, ok := inputBefore(inputs, previous.Time); ok && current.Time-input.T < idleSettle {
			continue
		}
		if previous.Cols != current.Cols {
			continue
		}
		previousRow, currentRow := lastRow(previous), lastRow(current)
		previousInset, currentInset := inset(previous), inset(current)
		switch {
		case previousInset != currentInset && previousRow == currentRow && previousRow >= 0:
			out = append(out, Evidence{
				Check:      CheckFooterDrift,
				Region:     screen.Region{X0: 0, Y0: currentRow, X1: current.Cols - 1, Y1: currentRow},
				Detail:     fmt.Sprintf("the last content row shifts horizontally from column %d to %d while the screen is otherwise idle", previousInset+1, currentInset+1),
				Severity:   1.5,
				Confidence: 0.5,
				Measured:   float64(currentInset - previousInset),
				GridID:     current.ID,
			})
			if len(out) >= 3 {
				break
			}
			continue
		case previousRow < 0 || currentRow < 0 || previousRow == currentRow:
			continue
		case previous.NonSpace() < 20 || current.NonSpace() < 20:
			continue
		}
		out = append(out, Evidence{
			Check:      CheckFooterDrift,
			Region:     screen.Region{X0: 0, Y0: min(previousRow, currentRow), X1: current.Cols - 1, Y1: max(previousRow, currentRow)},
			Detail:     fmt.Sprintf("the last content row moves from row %d to row %d while the screen is otherwise idle", previousRow+1, currentRow+1),
			Severity:   1.5,
			Confidence: 0.5,
			Measured:   float64(currentRow - previousRow),
			GridID:     current.ID,
		})
		if len(out) >= 3 {
			break
		}
	}
	return out
}

// stuckSpinners reports a spinner that is visible during a loading state but
// never advances: either the animation is broken or the work is wedged.
func stuckSpinners(grids []*screen.Grid) []Evidence {
	out := []Evidence{}
	if len(grids) < 4 {
		return out
	}
	last := grids[len(grids)-1]
	text := strings.ToLower(last.TrimmedText())
	if len(text) > loadingTextLimit*40 {
		text = text[:loadingTextLimit*40]
	}
	loading := strings.Contains(text, "loading") || strings.Contains(text, "starting") ||
		strings.Contains(text, "waiting") || strings.Contains(text, "fetching") ||
		strings.Contains(text, "connecting") || strings.Contains(text, "scanning") ||
		strings.Contains(text, "working")
	if !loading {
		return out
	}
	for _, spinner := range last.Structure().Spinners {
		changed := false
		for index := len(grids) - 2; index >= 0 && index >= len(grids)-6; index-- {
			if last.Time-grids[index].Time > 3*time.Second {
				break
			}
			cell, ok := grids[index].At(spinner.X, spinner.Y)
			if ok && string(cell.Char) != spinner.Char {
				changed = true
				break
			}
		}
		if changed {
			continue
		}
		out = append(out, Evidence{
			Check:      CheckStuckSpinner,
			Region:     screen.Region{X0: spinner.X, Y0: spinner.Y, X1: spinner.X, Y1: spinner.Y},
			Detail:     fmt.Sprintf("spinner glyph %q at %d,%d did not advance while the screen says it is loading", spinner.Char, spinner.X, spinner.Y),
			Severity:   2.5,
			Confidence: 0.65,
			GridID:     last.ID,
		})
		if len(out) >= 3 {
			break
		}
	}
	return out
}

// cadenceEvents measures the regularity of an animation burst. A spinner or
// progress animation that stutters is a real (and commonly reported) defect.
func cadenceEvents(grids []*screen.Grid) []Evidence {
	if len(grids) < minCadenceIntervals+2 {
		return nil
	}
	intervals := []float64{}
	for index := 1; index < len(grids); index++ {
		delta := grids[index].Time - grids[index-1].Time
		if delta <= 0 || delta > cadenceBurstMax {
			intervals = intervals[:0]
			continue
		}
		intervals = append(intervals, delta.Seconds())
		if len(intervals) >= minCadenceIntervals {
			mean := 0.0
			for _, value := range intervals {
				mean += value
			}
			mean /= float64(len(intervals))
			variance := 0.0
			for _, value := range intervals {
				variance += (value - mean) * (value - mean)
			}
			variance /= float64(len(intervals))
			if mean > 0 && math.Sqrt(variance)/mean > cadenceJitterLimit {
				return []Evidence{{
					Check:      CheckCadenceJitter,
					Region:     screen.Region{X0: 0, Y0: 0, X1: grids[index].Cols - 1, Y1: grids[index].RowsN - 1},
					Detail:     fmt.Sprintf("animation frames arrive irregularly: mean interval %.0fms with %.0f%% variation", mean*1000, 100*math.Sqrt(variance)/mean),
					Severity:   1.5,
					Confidence: 0.5,
					Measured:   math.Sqrt(variance) / mean,
					GridID:     grids[index].ID,
				}}
			}
			intervals = intervals[1:]
		}
	}
	return nil
}

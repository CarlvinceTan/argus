// Package sensors turns a screen into falsifiable evidence.
//
// Every check here is deterministic: code produces the predicate, the location
// and the measured value. Models never invent candidates; they only judge the
// ones code found (and, separately, answer open questions about the screen).
// That split is what keeps the report honest: when a finding is wrong you can
// point at the check that produced it.
package sensors

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/carlvincetan/argus/internal/screen"
)

// Check identifiers.
const (
	CheckBorderGap       = "border_gap"
	CheckBorderCrossing  = "border_crossing"
	CheckRaggedPanel     = "ragged_panel"
	CheckDuplicateLine   = "duplicate_line"
	CheckHardClip        = "hard_clip"
	CheckEllipsisMidline = "ellipsis_midline"
	CheckInvisibleText   = "invisible_text"
	CheckLowContrast     = "low_contrast"
	CheckEscapeArtifact  = "escape_artifact"
	CheckProgressLabel   = "progress_label_mismatch"
	CheckTableMisalign   = "table_misalignment"
	CheckLiteralJunk     = "literal_junk"
	CheckTruncationRoom  = "truncation_with_room"
)

// Evidence is one deterministic observation. It is a candidate, not a finding:
// the decision tier judges whether it is a real defect in context.
type Evidence struct {
	Check      string        `json:"check"`
	Region     screen.Region `json:"region"`
	Detail     string        `json:"detail"`
	Snippet    string        `json:"snippet,omitempty"`
	Severity   float64       `json:"severity_hint"`
	Confidence float64       `json:"confidence"`
	Measured   float64       `json:"measured,omitempty"`
	GridID     int           `json:"grid_id"`
}

// CheckOptions tune the deterministic pass.
type CheckOptions struct {
	MinContrast       float64
	LowContrastReport int
	MinDuplicateLen   int
	MaxEvidencePer    int
}

// DefaultCheckOptions are tuned to be quiet: each check demands a fairly
// specific signature before it produces evidence.
func DefaultCheckOptions() CheckOptions {
	return CheckOptions{
		MinContrast:       2.5,
		LowContrastReport: 3,
		MinDuplicateLen:   8,
		MaxEvidencePer:    6,
	}
}

// RunChecks runs every deterministic check against a grid.
func RunChecks(grid *screen.Grid, opts CheckOptions) []Evidence {
	out := []Evidence{}
	out = append(out, borderChecks(grid)...)
	out = append(out, lineChecks(grid, opts)...)
	out = append(out, colourChecks(grid, opts)...)
	out = append(out, artefactChecks(grid)...)
	out = append(out, layoutChecks(grid)...)
	out = append(out, truncationChecks(grid)...)
	return dedupeEvidence(out)
}

func borderChecks(grid *screen.Grid) []Evidence {
	out := []Evidence{}
	structure := grid.Structure()
	for _, panel := range structure.Panels {
		if !panel.Closed {
			out = append(out, Evidence{
				Check:      CheckBorderGap,
				Region:     panel.Region,
				Detail:     fmt.Sprintf("panel border is drawn on top and bottom but a side is interrupted (%.0f%% of the edge is drawn)", panel.Coverage*100),
				Severity:   2.0,
				Confidence: 0.8,
				Measured:   panel.Coverage,
				GridID:     grid.ID,
			})
		}
	}
	// Content crossing a vertical border of a closed panel.
	for _, panel := range structure.Panels {
		if !panel.Closed {
			continue
		}
		for y := panel.Y0 + 1; y < panel.Y1; y++ {
			for _, x := range []int{panel.X0, panel.X1} {
				cell, ok := grid.At(x, y)
				if !ok {
					continue
				}
				if isBoxRune(cell.Char) || cell.Char == ' ' {
					continue
				}
				out = append(out, Evidence{
					Check:      CheckBorderCrossing,
					Region:     screen.Region{X0: x, Y0: y, X1: x, Y1: y},
					Detail:     "text is drawn on top of a panel border",
					Snippet:    grid.Row(y)[max(0, x-12):min(grid.Cols, x+12)],
					Severity:   2.5,
					Confidence: 0.7,
					GridID:     grid.ID,
				})
			}
		}
	}
	// Ragged right edge inside a bordered panel.
	for _, panel := range structure.Panels {
		if !panel.Closed || panel.Width() < 12 {
			continue
		}
		rights := map[int]int{}
		for y := panel.Y0 + 1; y < panel.Y1; y++ {
			text := strings.TrimRight(grid.Row(y), " ")
			if len(text) == 0 {
				continue
			}
			end := len(text) - panel.X0
			rights[end]++
		}
		if len(rights) >= 3 {
			out = append(out, Evidence{
				Check:      CheckRaggedPanel,
				Region:     panel.Region,
				Detail:     fmt.Sprintf("content inside the panel ends at %d different columns (%v), so the right edge is ragged", len(rights), sortedKeys(rights)),
				Severity:   1.5,
				Confidence: 0.5,
				GridID:     grid.ID,
			})
		}
	}
	return out
}

func lineChecks(grid *screen.Grid, opts CheckOptions) []Evidence {
	out := []Evidence{}
	previous := ""
	previousY := -1
	duplicates := 0
	for y := 0; y < grid.RowsN; y++ {
		row := strings.TrimRight(grid.Row(y), " ")
		trimmed := strings.TrimSpace(row)
		if trimmed == "" {
			previous = ""
			continue
		}
		if previousY == y-1 && trimmed == previous && len(trimmed) >= opts.MinDuplicateLen && !isRuleRow(trimmed) {
			out = append(out, Evidence{
				Check:      CheckDuplicateLine,
				Region:     screen.Region{X0: leadingSpaces(row), Y0: y - 1, X1: len(row) - 1, Y1: y},
				Detail:     fmt.Sprintf("the same line appears on rows %d and %d", y, y+1),
				Snippet:    trimmed,
				Severity:   2.0,
				Confidence: 0.6,
				GridID:     grid.ID,
			})
			duplicates++
			if duplicates >= opts.MaxEvidencePer {
				return out
			}
		}
		previous = trimmed
		previousY = y
	}

	for y := 0; y < grid.RowsN; y++ {
		row := grid.Row(y)
		trimmedRight := strings.TrimRight(row, " ")
		if len(trimmedRight) < 4 {
			continue
		}
		runes := []rune(trimmedRight)
		lastChar := runes[len(runes)-1]
		// A clip looks like a long unbroken token (a path, URL or identifier)
		// running into the last column. Text with spaces near the end is much
		// more likely to be a status bar that simply fills the width.
		tailStart := len(runes) - 12
		if tailStart < 0 {
			tailStart = 0
		}
		unbroken := !strings.ContainsRune(string(runes[tailStart:]), ' ')
		// A right-aligned value legitimately ends at the last column, so it is not
		// evidence of clipping: the gap in front of the token is what marks it as
		// right-aligned. A token that follows text directly and runs off the edge
		// is a clip candidate. Rows that already carry a truncation marker are
		// covered by truncation_with_room, which checks whether room was wasted.
		freeOfMarkers := true
		for _, marker := range truncationMarkers {
			if strings.Contains(row, marker) {
				freeOfMarkers = false
				break
			}
		}
		leftAligned := gapBeforeLastToken(row) < rightAlignGap
		if freeOfMarkers && leftAligned && len(runes) >= grid.Cols-1 && unbroken && !isBoxRune(lastChar) &&
			!strings.ContainsRune(".!?)]}:;,-–—…»\"'`", lastChar) && !unicode.IsSpace(lastChar) {
			out = append(out, Evidence{
				Check:      CheckHardClip,
				Region:     screen.Region{X0: max(0, grid.Cols-30), Y0: y, X1: grid.Cols - 1, Y1: y},
				Detail:     "an unbroken token runs into the last column mid-word, consistent with clipping",
				Snippet:    trimmedRight,
				Severity:   2.0,
				Confidence: 0.45,
				GridID:     grid.ID,
			})
		}
		if index := strings.IndexRune(row, '…'); index >= 0 && index < len(trimmedRight)-1 {
			tail := strings.TrimSpace(row[index+len("…"):])
			if tail != "" && !isBoxRune(rune(row[len(trimmedRight)-1])) {
				out = append(out, Evidence{
					Check:      CheckEllipsisMidline,
					Region:     screen.Region{X0: index, Y0: y, X1: min(grid.Cols, index+24) - 1, Y1: y},
					Detail:     "an ellipsis appears before the end of the row, so the truncated value may be followed by unrelated text",
					Snippet:    row,
					Severity:   1.5,
					Confidence: 0.4,
					GridID:     grid.ID,
				})
			}
		}
	}
	return out
}

func colourChecks(grid *screen.Grid, opts CheckOptions) []Evidence {
	out := []Evidence{}
	theme := screen.DefaultTheme()
	type run struct {
		region screen.Region
		text   strings.Builder
	}
	lowContrastRuns := []run{}
	invisible := []run{}

	for y := 0; y < grid.RowsN; y++ {
		startX := -1
		builder := strings.Builder{}
		for x := 0; x <= grid.Cols; x++ {
			var cell screen.Cell
			ok := x < grid.Cols
			if ok {
				cell, ok = grid.At(x, y)
			}
			problem := false
			if ok && cell.Char != ' ' && !isBoxRune(cell.Char) {
				fg := theme.Resolve(cell.FG, false)
				bg := theme.Resolve(cell.BG, true)
				if cell.Reverse {
					fg, bg = bg, fg
				}
				contrast := screen.ContrastRatio(fg, bg)
				problem = contrast < opts.MinContrast
				if problem {
					builder.WriteRune(cell.Char)
				}
			}
			if problem && startX < 0 {
				startX = x
			}
			if !problem && startX >= 0 {
				region := screen.Region{X0: startX, Y0: y, X1: x - 1, Y1: y}
				entry := lowContrastRuns
				_ = entry
				text := builder.String()
				if strings.TrimSpace(text) != "" {
					lowContrastRuns = append(lowContrastRuns, run{region: region, text: strings.Builder{}})
					lowContrastRuns[len(lowContrastRuns)-1].text.WriteString(text)
				}
				builder.Reset()
				startX = -1
			}
		}
	}

	for _, entry := range lowContrastRuns {
		if entry.text.Len() == 0 {
			continue
		}
		if len(out) >= opts.LowContrastReport {
			break
		}
		out = append(out, Evidence{
			Check:      CheckLowContrast,
			Region:     entry.region,
			Detail:     fmt.Sprintf("text %q renders with a contrast ratio below %.1f", truncateText(entry.text.String(), 40), opts.MinContrast),
			Snippet:    entry.text.String(),
			Severity:   1.5,
			Confidence: 0.7,
			GridID:     grid.ID,
		})
	}

	// Whole-row invisibility: foreground equals background.
	for y := 0; y < grid.RowsN; y++ {
		start := -1
		text := strings.Builder{}
		for x := 0; x <= grid.Cols; x++ {
			var cell screen.Cell
			ok := x < grid.Cols
			if ok {
				cell, ok = grid.At(x, y)
			}
			invisibleHere := false
			if ok && cell.Char != ' ' {
				invisibleHere = cell.FG == cell.BG && !screen.IsDefaultFG(cell.FG) && !screen.IsDefaultBG(cell.FG)
			}
			if invisibleHere && start < 0 {
				start = x
			}
			if invisibleHere {
				text.WriteRune(cell.Char)
			}
			if !invisibleHere && start >= 0 {
				invisible = append(invisible, run{
					region: screen.Region{X0: start, Y0: y, X1: x - 1, Y1: y},
					text:   wrapBuilder(text.String()),
				})
				text.Reset()
				start = -1
			}
		}
	}
	for _, entry := range invisible {
		if entry.text.Len() == 0 {
			continue
		}
		out = append(out, Evidence{
			Check:      CheckInvisibleText,
			Region:     entry.region,
			Detail:     fmt.Sprintf("text %q has identical foreground and background colours, so it cannot be read", truncateText(entry.text.String(), 40)),
			Snippet:    entry.text.String(),
			Severity:   3.0,
			Confidence: 0.85,
			GridID:     grid.ID,
		})
	}
	return out
}

var (
	escapeArtifactPattern = regexp.MustCompile(`(\[[0-9;]{2,}m|\[0m|\[38;5|\[48;5|\]0;|\x1b|\^\[\[|%!s\(|%!d\(|%!v\(|<nil>|\b%s\b)`)
	literalJunkPattern    = regexp.MustCompile(`(\x{FFFD}|\\u[0-9a-fA-F]{4}|\bundefined\b|\bNaN\b|\bnull\b\s*$|\btrue\b\s*\|\s*\bfalse\b)`)
)

func artefactChecks(grid *screen.Grid) []Evidence {
	out := []Evidence{}
	for y := 0; y < grid.RowsN; y++ {
		row := grid.Row(y)
		if strings.TrimSpace(row) == "" {
			continue
		}
		if match := escapeArtifactPattern.FindString(row); match != "" {
			column := strings.Index(row, match)
			out = append(out, Evidence{
				Check:      CheckEscapeArtifact,
				Region:     screen.Region{X0: max(0, column-8), Y0: y, X1: min(grid.Cols, column+len(match)+8) - 1, Y1: y},
				Detail:     fmt.Sprintf("raw escape or format artifact %q is printed as text", match),
				Snippet:    strings.TrimSpace(row),
				Severity:   2.5,
				Confidence: 0.8,
				GridID:     grid.ID,
			})
			continue
		}
		if match := literalJunkPattern.FindString(row); match != "" {
			column := strings.Index(row, match)
			out = append(out, Evidence{
				Check:      CheckLiteralJunk,
				Region:     screen.Region{X0: max(0, column-8), Y0: y, X1: min(grid.Cols, column+len(match)+8) - 1, Y1: y},
				Detail:     fmt.Sprintf("unrendered placeholder %q appears in the interface", match),
				Snippet:    strings.TrimSpace(row),
				Severity:   2.0,
				Confidence: 0.6,
				GridID:     grid.ID,
			})
		}
	}
	return out
}

// truncationMarkers are the ways terminal programs indicate a clipped value.
var truncationMarkers = []string{"…", "··", "..", "..."}

// truncationChecks finds values that were truncated even though the row had
// room left over. That is the signature of a real layout bug: the program
// clipped text for its own convenience, not because the terminal ran out of
// space. The room is measured as the gap immediately in front of the value
// (a right-aligned value) or the trailing space after it, because a value that
// genuinely ran out of room has neither.
func truncationChecks(grid *screen.Grid) []Evidence {
	out := []Evidence{}
	for y := 0; y < grid.RowsN; y++ {
		runes := []rune(grid.Row(y))
		if len(runes) == 0 {
			continue
		}
		firstNonSpace, lastNonSpace := -1, -1
		for index, char := range runes {
			if char == ' ' {
				continue
			}
			if firstNonSpace < 0 {
				firstNonSpace = index
			}
			lastNonSpace = index
		}
		if firstNonSpace < 0 {
			continue
		}
		row := string(runes)
		for _, marker := range truncationMarkers {
			markerIndex := strings.Index(row, marker)
			if markerIndex < 0 {
				continue
			}
			// The marker must sit at the end of the visible value.
			if strings.TrimSpace(row[markerIndex:]) != marker {
				continue
			}
			before := []rune(row[:markerIndex])
			if len(before) == 0 || before[len(before)-1] == ' ' {
				continue
			}
			gap := gapBeforeLastToken(row)
			trailing := len(runes) - 1 - lastNonSpace
			spare := gap
			if trailing > spare {
				spare = trailing
			}
			if spare < len([]rune(marker))+4 {
				continue // the value really did run out of room
			}
			out = append(out, Evidence{
				Check:      CheckTruncationRoom,
				Region:     screen.Region{X0: firstNonSpace, Y0: y, X1: lastNonSpace, Y1: y},
				Detail:     fmt.Sprintf("value %q is truncated with %q while %d columns in front of it are empty, so it could have been shown in full", strings.TrimSpace(string(before)), marker, spare),
				Snippet:    strings.TrimSpace(row),
				Severity:   2.0,
				Confidence: 0.65,
				Measured:   float64(spare),
				GridID:     grid.ID,
			})
			break
		}
	}
	return out
}

func layoutChecks(grid *screen.Grid) []Evidence {
	out := []Evidence{}
	for _, bar := range grid.Structure().Bars {
		if bar.Percent == nil {
			continue
		}
		expected := float64(*bar.Percent) / 100
		delta := bar.Fraction - expected
		if delta < 0 {
			delta = -delta
		}
		if delta >= 0.15 {
			out = append(out, Evidence{
				Check:      CheckProgressLabel,
				Region:     bar.Region,
				Detail:     fmt.Sprintf("bar is %.0f%% filled while its label reads %d%%", bar.Fraction*100, *bar.Percent),
				Snippet:    strings.TrimSpace(grid.Row(bar.Y0)),
				Severity:   2.0,
				Confidence: 0.7,
				Measured:   bar.Fraction,
				GridID:     grid.ID,
			})
		}
	}
	out = append(out, tableMisalignment(grid)...)
	return out
}

// tableMisalignment finds blocks of two or more columns and reports rows whose
// column starts drift away from the block's dominant alignment.
func tableMisalignment(grid *screen.Grid) []Evidence {
	out := []Evidence{}
	type rowGaps struct {
		y      int
		starts []int
	}
	rows := []rowGaps{}
	for y := 0; y < grid.RowsN; y++ {
		row := grid.Row(y)
		gaps := gapStarts(row)
		if len(gaps) >= 2 {
			rows = append(rows, rowGaps{y: y, starts: gaps})
		}
	}
	if len(rows) < 4 {
		return out
	}
	// Dominant column positions across the block.
	counts := map[int]int{}
	for _, row := range rows {
		for _, start := range row.starts {
			counts[start]++
		}
	}
	dominant := []int{}
	for start, count := range counts {
		if count >= len(rows)/2 && count >= 2 {
			dominant = append(dominant, start)
		}
	}
	if len(dominant) < 2 {
		return out
	}
	sortInts(dominant)
	for _, row := range rows {
		off := 0
		for _, start := range row.starts {
			if !nearAny(start, dominant, 1) {
				off++
			}
		}
		if off > 0 {
			out = append(out, Evidence{
				Check:      CheckTableMisalign,
				Region:     screen.Region{X0: 0, Y0: row.y, X1: grid.Cols - 1, Y1: row.y},
				Detail:     fmt.Sprintf("row %d has %d column start(s) that do not line up with the dominant columns %v", row.y+1, off, dominant),
				Snippet:    strings.TrimSpace(grid.Row(row.y)),
				Severity:   1.5,
				Confidence: 0.35,
				GridID:     grid.ID,
			})
		}
		if len(out) >= 3 {
			break
		}
	}
	return out
}

func gapStarts(row string) []int {
	runes := []rune(row)
	starts := []int{}
	for index := 0; index < len(runes); index++ {
		if runes[index] != ' ' {
			continue
		}
		run := 0
		for index+run < len(runes) && runes[index+run] == ' ' {
			run++
		}
		if run >= 2 && index+run < len(runes) {
			starts = append(starts, index+run)
		}
		index += run
	}
	return starts
}

func isBoxRune(char rune) bool {
	return strings.ContainsRune(boxRunes, char)
}

const boxRunes = "─│┌┐└┘├┤┬┴┼━┃┏┓┗┛╔╗╚╝╭╮╰╯═║╒╓╕╖╘╙╛╜╞╟╠╡╢╣╤╥╦╧╨╩╪╫╬▏▕┄┅┆┇┊┋╌╍"

func isRuleRow(text string) bool {
	rule := 0
	for _, char := range text {
		if isBoxRune(char) {
			rule++
		}
	}
	return rule >= len([]rune(text))*3/4
}

// rightAlignGap is the number of spaces in front of a token that marks it as
// right-aligned rather than as the continuation of running text.
const rightAlignGap = 8

// gapBeforeLastToken counts the spaces immediately before the final run of
// non-space characters on a row.
func gapBeforeLastToken(row string) int {
	runes := []rune(strings.TrimRight(row, " "))
	end := len(runes)
	start := end
	for start > 0 && runes[start-1] != ' ' {
		start--
	}
	gap := 0
	for index := start - 1; index >= 0 && runes[index] == ' '; index-- {
		gap++
	}
	return gap
}

func leadingSpaces(row string) int {
	return len(row) - len(strings.TrimLeft(row, " "))
}

func dedupeEvidence(items []Evidence) []Evidence {
	seen := map[string]bool{}
	out := make([]Evidence, 0, len(items))
	for _, item := range items {
		key := fmt.Sprintf("%s|%s|%s", item.Check, item.Region, item.Snippet)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item)
	}
	return out
}

func sortedKeys(counts map[int]int) []int {
	keys := make([]int, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sortInts(keys)
	return keys
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func nearAny(value int, targets []int, tolerance int) bool {
	for _, target := range targets {
		if value-target <= tolerance && target-value <= tolerance {
			return true
		}
	}
	return false
}

func truncateText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}

func wrapBuilder(text string) strings.Builder {
	builder := strings.Builder{}
	builder.WriteString(text)
	return builder
}

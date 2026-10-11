package sensors

import (
	"fmt"
	"sort"
	"strings"

	"github.com/carlvincetan/argus/internal/screen"
)

// Structural check identifiers.
const (
	CheckNesting = "structure_nesting"
)

// RunStructuralChecks measures how the screen is put together rather than what it
// contains. These are judgements about organisation -- nesting, competing focus,
// the same value repeated in several panels -- and they belong next to the
// screen, because a reviewer needs to see what is nested inside what in order to
// say whether the nesting helps or hurts.
func RunStructuralChecks(grid *screen.Grid) []Evidence {
	out := []Evidence{}
	out = append(out, nestingEvidence(grid)...)
	out = append(out, competingFocusEvidence(grid)...)
	out = append(out, repetitionEvidence(grid)...)
	return out
}

// nestingEvidence reports boxes drawn inside boxes inside boxes.
func nestingEvidence(grid *screen.Grid) []Evidence {
	rectangles := screen.NestingChain(grid)
	if len(rectangles) < 3 {
		return nil
	}
	// Describe the chain so the reviewer can judge the structure itself.
	descriptions := make([]string, 0, len(rectangles))
	for index, region := range rectangles {
		title := screen.BoxTitle(grid, region)
		if title == "" {
			title = fmt.Sprintf("box %d", index+1)
		}
		descriptions = append(descriptions, fmt.Sprintf("%s (%dx%d at %s)", title, region.Width(), region.Height(), region))
	}
	deepest := rectangles[len(rectangles)-1]
	return []Evidence{{
		Check:      CheckNesting,
		Region:     deepest,
		Detail:     fmt.Sprintf("content is nested %d boxes deep: %s", len(rectangles), strings.Join(descriptions, " > ")),
		Severity:   2.0,
		Confidence: 0.75,
		Measured:   float64(len(rectangles)),
		GridID:     grid.ID,
	}}
}

// competingFocusEvidence reports several highlighted regions at once, which makes
// it unclear which one the keyboard acts on.
func competingFocusEvidence(grid *screen.Grid) []Evidence {
	rows := map[int]screen.Region{}
	for _, highlight := range grid.Structure().Highlights {
		if highlight.Kind == "reverse" {
			rows[highlight.Y0] = highlight.Region
		}
	}
	if len(rows) < 3 {
		return nil
	}
	sorted := make([]int, 0, len(rows))
	for row := range rows {
		sorted = append(sorted, row)
	}
	sort.Ints(sorted)
	region := rows[sorted[0]]
	for _, row := range sorted[1:] {
		other := rows[row]
		region.X0 = minInt(region.X0, other.X0)
		region.X1 = maxInt(region.X1, other.X1)
		region.Y1 = maxInt(region.Y1, other.Y1)
	}
	return []Evidence{{
		Check:      "structure_competing_focus",
		Region:     region,
		Detail:     fmt.Sprintf("%d separate regions are highlighted at once, so it is unclear which one the keyboard is acting on", len(rows)),
		Severity:   2.0,
		Confidence: 0.55,
		Measured:   float64(len(rows)),
		GridID:     grid.ID,
	}}
}

// repetitionEvidence reports the same value shown in three or more places, which
// usually means two panels describe the same thing.
func repetitionEvidence(grid *screen.Grid) []Evidence {
	counts := map[string][]screen.Region{}
	for y := 0; y < grid.RowsN; y++ {
		row := grid.Row(y)
		for _, token := range strings.Fields(row) {
			token = strings.Trim(token, "│┃║|,[]():")
			if len([]rune(token)) < 10 {
				continue
			}
			counts[token] = append(counts[token], screen.Region{X0: 0, Y0: y, X1: grid.Cols - 1, Y1: y})
		}
	}
	best, bestCount := "", 0
	for token, places := range counts {
		if len(places) > bestCount {
			best, bestCount = token, len(places)
		}
	}
	if bestCount < 3 {
		return nil
	}
	places := counts[best]
	return []Evidence{{
		Check:      "structure_repetition",
		Region:     places[0],
		Detail:     fmt.Sprintf("the value %q appears on %d rows of this screen, so two panels may be describing the same thing", best, bestCount),
		Severity:   1.5,
		Confidence: 0.5,
		Measured:   float64(bestCount),
		GridID:     grid.ID,
	}}
}

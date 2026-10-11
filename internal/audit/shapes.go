package audit

import (
	"sort"
	"strings"
	"unicode"

	"github.com/carlvincetan/argus/internal/screen"
)

// contentRunes strips the cell decoration a terminal UI draws around content:
// vertical borders, bullets and the padding between them.
func contentRunes(row string) []rune {
	trimmed := strings.TrimSpace(row)
	trimmed = strings.Trim(trimmed, "│┃║|╎┆┇┊┋")
	return []rune(strings.TrimSpace(trimmed))
}

// rowShape is what one row of a screen is, which is all the shape detection
// needs to know. Ambiguous rows are resolved by looking at the run they are in,
// not by one row at a time: "prod-us-2   connected   38 ms" is a field on its own
// and a table row in a column of them.
type rowShape int

const (
	shapeEmpty rowShape = iota
	shapeDecoration
	shapeHint
	shapeField
	shapeRecord
)

func classifyRow(grid *screen.Grid, y int, hints map[int]bool) rowShape {
	row := grid.Row(y)
	if strings.TrimSpace(row) == "" {
		return shapeEmpty
	}
	if hints[y] {
		return shapeHint
	}
	runes := contentRunes(row)
	if len(runes) < 2 {
		return shapeDecoration
	}
	text := string(runes)
	// A row made only of box drawing is a border, not content.
	if isDecorationOnly(text) {
		return shapeDecoration
	}
	if isFieldRow(text) {
		return shapeField
	}
	if isRecordRow(text) {
		return shapeRecord
	}
	return shapeDecoration
}

func isDecorationOnly(text string) bool {
	for _, char := range text {
		if !strings.ContainsRune("─━═┄┅┬┴┼*+-=_.· ", char) {
			return false
		}
	}
	return true
}

// isFieldRow recognises "Label: value" and "Label   value" pairs where the label
// is a clean, short noun phrase and the value is a short value.
func isFieldRow(text string) bool {
	label, value, ok := splitLabelValue(text)
	if !ok {
		return false
	}
	if label == "" || value == "" {
		return false
	}
	if strings.ContainsAny(label, "0123456789>:-—") || strings.ContainsAny(value, "│┃║|") {
		return false
	}
	if len(strings.Fields(label)) > 3 || len(strings.Fields(value)) > 4 {
		return false
	}
	return true
}

// isRecordRow recognises a row of a table or list: a short entry followed by
// aligned columns, or a row introduced by a selection marker.
func isRecordRow(text string) bool {
	runes := []rune(text)
	if len(runes) == 0 || len(runes) > 96 {
		return false
	}
	first := runes[0]
	switch first {
	case '>', '-', '*', '•', '▌', '▸', '→':
		return true
	}
	if unicode.IsDigit(first) {
		return true
	}
	if len(strings.Fields(text)) < 2 {
		return false
	}
	return firstGap(text) >= 0
}

// firstGap returns the column where the first run of two or more spaces starts,
// which is the column alignment of a table.
func firstGap(text string) int {
	runes := []rune(text)
	for index := 0; index+1 < len(runes); index++ {
		if runes[index] == ' ' && runes[index+1] == ' ' {
			return index
		}
	}
	return -1
}

// splitLabelValue recognises "Label: value" and "Label   value" rows.
func splitLabelValue(row string) (string, string, bool) {
	if label, value, found := strings.Cut(row, ":"); found {
		label = strings.TrimSpace(label)
		value = strings.TrimSpace(value)
		if label != "" && value != "" && len(label) <= 24 && len(strings.Fields(label)) <= 3 {
			return label, value, true
		}
	}
	fields := strings.SplitN(row, "  ", 2)
	if len(fields) == 2 {
		label := strings.TrimSpace(fields[0])
		value := strings.TrimSpace(fields[1])
		if label != "" && value != "" && len(label) <= 24 && len(strings.Fields(label)) <= 2 {
			return label, value, true
		}
	}
	return "", "", false
}

// listShape estimates how list-like a screen is: how many consecutive rows share
// a shape and a column, and whether one of them is selected.
func listShape(grid *screen.Grid) (rows int, selected bool) {
	return listShapeDetail(grid)
}

func listShapeDetail(grid *screen.Grid) (rows int, selected bool) {
	hints := keyHintRows(grid)
	best, run, runColumn := 0, 0, -2
	for y := 0; y < grid.RowsN; y++ {
		if classifyRow(grid, y, hints) != shapeRecord {
			run, runColumn = 0, -2
			continue
		}
		text := string(contentRunes(grid.Row(y)))
		column := firstGap(text)
		if column < 0 {
			column = 0
		}
		if run == 0 {
			runColumn = column
		}
		if absInt(column-runColumn) > 1 {
			run, runColumn = 1, column
			continue
		}
		run++
		if run > best {
			best = run
		}
	}
	structure := grid.Structure()
	for _, highlight := range structure.Highlights {
		if highlight.Kind == "reverse" {
			selected = true
			break
		}
	}
	return best, selected
}

// detailShape counts consecutive field rows whose labels line up, which is what
// a detail view for one entity looks like.
func detailShape(grid *screen.Grid) (pairs int, labels []string) {
	hints := keyHintRows(grid)
	best, run, runColumn := 0, 0, -2
	for y := 0; y < grid.RowsN; y++ {
		if classifyRow(grid, y, hints) != shapeField {
			run, runColumn = 0, -2
			continue
		}
		text := string(contentRunes(grid.Row(y)))
		column := valueColumn(text)
		if run == 0 {
			runColumn = column
		}
		if column != runColumn {
			run, runColumn = 1, column
			continue
		}
		run++
		if run > best {
			best = run
		}
		if len(labels) < 6 {
			if label, _, ok := splitLabelValue(text); ok {
				labels = append(labels, label)
			}
		}
	}
	return best, labels
}

// valueColumn is where the value of a field row begins. For "Label: value" rows
// that is after the colon, because well-built detail views align their values
// rather than their colons.
func valueColumn(text string) int {
	if index := strings.Index(text, ":"); index >= 0 {
		rest := []rune(text[index+1:])
		for offset, char := range rest {
			if char != ' ' {
				return index + 1 + offset
			}
		}
		return index + 1
	}
	return firstGap(text)
}

// keyHintRows marks the rows an interface uses to advertise its controls, which
// are text but not content.
func keyHintRows(grid *screen.Grid) map[int]bool {
	out := map[int]bool{}
	counts := map[int]int{}
	for _, hint := range grid.Structure().KeyHints {
		counts[hint.Y]++
	}
	for y, count := range counts {
		if count >= 2 {
			out[y] = true
		}
	}
	return out
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// inputField returns the region of a focused text field: a small bordered area
// containing the cursor.
func inputField(grid *screen.Grid) (screen.Region, bool) {
	cursorX, cursorY := grid.CursorX, grid.CursorY
	if !grid.CursorVisible || cursorY < 0 || cursorY >= grid.RowsN {
		return screen.Region{}, false
	}
	for _, panel := range grid.Structure().Panels {
		if !panel.Closed || panel.Height() > 4 {
			continue
		}
		if panel.Contains(cursorX, cursorY) {
			return panel.Region, true
		}
	}
	return screen.Region{}, false
}

// inferItemKind names what the rows of a list are about, using the panel title
// above the list or the most common leading word of its rows.
func inferItemKind(grid *screen.Grid) string {
	counts := map[string]int{}
	hints := keyHintRows(grid)
	for y := 0; y < grid.RowsN; y++ {
		if classifyRow(grid, y, hints) != shapeRecord {
			continue
		}
		trimmed := string(contentRunes(grid.Row(y)))
		fields := strings.Fields(strings.TrimLeft(trimmed, ">-*•▌▸→[]()1234567890. "))
		if len(fields) == 0 {
			continue
		}
		word := strings.ToLower(strings.Trim(fields[0], "[]():,"))
		if len(word) < 3 {
			continue
		}
		counts[word]++
	}
	best, bestCount := "", 0
	for word, count := range counts {
		if count > bestCount {
			best, bestCount = word, count
		}
	}
	if bestCount < 2 {
		return ""
	}
	return best
}

// nestingDepth counts how many boxes are drawn inside one another, which is the
// measurable half of "panels inside panels inside panels".
//
// It is measured from corner characters rather than from the heuristic panel
// pairing used by the visual checks: nesting is a geometry question, and a
// rectangle defined by ┌…┐ / └…┘ is unambiguous.
func nestingDepth(grid *screen.Grid) (int, screen.Region) {
	// Two independent measurements: corner-derived rectangles are exact, the
	// heuristic panel pairing survives a screen whose corners were clipped by a
	// resize. The deeper answer wins, because a missed nesting level is exactly
	// the defect this is looking for.
	chain := screen.NestingChain(grid)
	cornerDepth, cornerRegion := len(chain), screen.Region{}
	if len(chain) > 0 {
		cornerRegion = chain[len(chain)-1]
	}
	heuristicDepth, heuristicRegion := nestingFromPanels(grid)
	if heuristicDepth > cornerDepth {
		return heuristicDepth, heuristicRegion
	}
	return cornerDepth, cornerRegion
}

// nestingFromPanels uses the visual checks' panel pairing as a fallback.
func nestingFromPanels(grid *screen.Grid) (int, screen.Region) {
	closed := []screen.Panel{}
	for _, panel := range grid.Structure().Panels {
		if panel.Closed && panel.Width() > 6 && panel.Height() > 2 {
			closed = append(closed, panel)
		}
	}
	sort.SliceStable(closed, func(a, b int) bool {
		return closed[a].Width()*closed[a].Height() > closed[b].Width()*closed[b].Height()
	})
	depth := 0
	deepest := screen.Region{}
	for index, outer := range closed {
		current := 1
		region := outer.Region
		for _, inner := range closed[index+1:] {
			if region.Contains(inner.X0, inner.Y0) && region.Contains(inner.X1, inner.Y1) {
				current++
				region = inner.Region
			}
		}
		if current > depth {
			depth, deepest = current, region
		}
	}
	return depth, deepest
}

// deadEnd reports whether a screen offers nothing but ways out: no control other

// deadEnd reports whether a screen offers nothing but ways out: no control other
// than quitting, going back or escaping.
func deadEnd(grid *screen.Grid) bool {
	hints := grid.Structure().KeyHints
	if len(hints) == 0 {
		return false
	}
	forward := 0
	for _, hint := range hints {
		label := strings.ToLower(hint.Label + " " + hint.Key)
		if containsAny(label, []string{"quit", "exit", "back", "esc", "close", "cancel", "dismiss"}) {
			continue
		}
		forward++
	}
	return forward == 0
}

func containsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func leadingSpaces(row string) int {
	return len(row) - len(strings.TrimLeft(row, " "))
}

// competingSelections counts how many separate highlighted regions are on screen
// at once. Several simultaneous selections make it hard to know what is focused.
func competingSelections(grid *screen.Grid) int {
	rows := map[int]bool{}
	for _, highlight := range grid.Structure().Highlights {
		if highlight.Kind == "reverse" {
			rows[highlight.Y0] = true
		}
	}
	return len(rows)
}

// repeatedValues finds a value printed several times on one screen, which is the
// measurable form of "the same information shown in three panels".
func repeatedValues(grid *screen.Grid, minLength int) []RepeatedValue {
	counts := map[string]int{}
	for y := 0; y < grid.RowsN; y++ {
		row := strings.TrimSpace(grid.Row(y))
		if row == "" {
			continue
		}
		if _, value, ok := splitLabelValue(string(contentRunes(row))); ok && len(value) >= minLength {
			counts[strings.ToLower(value)]++
			continue
		}
		for _, token := range strings.Fields(row) {
			token = strings.Trim(token, "|│,")
			if len(token) >= minLength {
				counts[strings.ToLower(token)]++
			}
		}
	}
	out := []RepeatedValue{}
	for value, count := range counts {
		if count >= 3 {
			out = append(out, RepeatedValue{Value: value, Count: count, Screen: screen.ScreenName(grid)})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Count > out[b].Count })
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

package screen

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const (
	minRuleLength    = 12
	minVerticalRun   = 3
	minHighlightRun  = 3
	minBarLength     = 5
	maxKeyHints      = 24
	maxHighlights    = 48
	maxTextRegions   = 32
	maxRowsInSummary = 60
)

// Rule is a horizontal or vertical line drawn with box characters.
type Rule struct {
	Kind   string `json:"kind"` // "h" or "v"
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Length int    `json:"len"`
}

// Panel is a rectangle inferred from paired horizontal rules.
type Panel struct {
	Region
	Closed   bool    `json:"closed"`
	Coverage float64 `json:"coverage"`
}

// KeyHint is a key binding advertised by the UI.
type KeyHint struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
}

// Highlight is a selected or painted run of cells.
type Highlight struct {
	Region
	Kind string `json:"kind"` // "reverse" or "bg"
	Text string `json:"text"`
}

// Bar is a progress-bar-like run of block characters.
type Bar struct {
	Region
	Fraction float64 `json:"fraction"`
	Label    string  `json:"label,omitempty"`
	Percent  *int    `json:"percent,omitempty"`
}

// Spinner is a position where spinner-like glyphs appear.
type Spinner struct {
	X    int    `json:"x"`
	Y    int    `json:"y"`
	Char string `json:"char"`
}

// TextRegion is a contiguous block of non-blank rows.
type TextRegion struct {
	Region
	Lines int `json:"lines"`
}

// Structure is everything derived from a grid in one pass.
type Structure struct {
	Panels      []Panel      `json:"panels"`
	HRules      []Rule       `json:"h_rules"`
	VRules      []Rule       `json:"v_rules"`
	KeyHints    []KeyHint    `json:"key_hints"`
	Highlights  []Highlight  `json:"highlights"`
	Bars        []Bar        `json:"bars"`
	Spinners    []Spinner    `json:"spinners"`
	TextRegions []TextRegion `json:"text_regions"`
	ContentRows []int        `json:"content_rows"`
	NonSpace    int          `json:"non_space"`
	Cursor      [2]int       `json:"cursor"`
}

const (
	hRuleChars = "─━═╌╍┄┅"
	vRuleChars = "│┃║┆┇┊┋"
	// Corners and tees can extend a rule but never start one: that keeps a row
	// containing only two corners from looking like a horizontal rule.
	cornerChars = "┌┐└┘├┤┬┴┼╭╮╰╯╔╗╚╝╠╣╦╩╬╞╟╡╢"
	barChars    = "█▓▒░▉▊▋▌▍▎▏"
)

// horizontalRules finds runs of horizontal box characters, allowing corners to
// extend a run so that "┌─ title ──┐" is one rule rather than two fragments.
func horizontalRules(row string, y int) []Rule {
	rules := []Rule{}
	runes := []rune(row)
	start, end, pure := -1, -1, 0
	flush := func() {
		if start >= 0 && pure >= minRuleLength {
			rules = append(rules, Rule{Kind: "h", X: start, Y: y, Length: end - start + 1})
		}
		start, end, pure = -1, -1, 0
	}
	for x, char := range runes {
		isCorner := strings.ContainsRune(cornerChars, char)
		startsRule := isCorner && x+1 < len(runes) && strings.ContainsRune(hRuleChars, runes[x+1])
		switch {
		case strings.ContainsRune(hRuleChars, char) || startsRule:
			if start < 0 {
				start = x
			}
			if !isCorner {
				pure++
			}
			end = x
		case start >= 0 && isCorner:
			end = x
		default:
			flush()
		}
	}
	flush()
	return rules
}

// DeriveStructure computes panels, rules, hints, highlights and bars.
func DeriveStructure(grid *Grid) *Structure {
	structure := &Structure{
		Panels:      []Panel{},
		HRules:      []Rule{},
		VRules:      []Rule{},
		KeyHints:    []KeyHint{},
		Highlights:  []Highlight{},
		Bars:        []Bar{},
		Spinners:    []Spinner{},
		TextRegions: []TextRegion{},
		ContentRows: []int{},
		NonSpace:    grid.NonSpace(),
		Cursor:      [2]int{grid.CursorX, grid.CursorY},
	}
	for y := 0; y < grid.RowsN; y++ {
		row := grid.Rows[y]
		if strings.TrimSpace(row) != "" {
			structure.ContentRows = append(structure.ContentRows, y)
		}
		structure.HRules = append(structure.HRules, horizontalRules(row, y)...)
	}
	for x := 0; x < grid.Cols; x++ {
		structure.VRules = append(structure.VRules, verticalRules(grid, x)...)
	}
	structure.Panels = pairPanels(structure.HRules, structure.VRules, grid)
	structure.KeyHints = keyHints(grid)
	structure.Highlights = highlights(grid)
	structure.Bars = bars(grid)
	structure.Spinners = spinners(grid)
	structure.TextRegions = textRegions(grid)
	return structure
}

func verticalRules(grid *Grid, x int) []Rule {
	rules := []Rule{}
	runStart := -1
	for y := 0; y < grid.RowsN; y++ {
		char := runeAt(grid, x, y)
		isRule := strings.ContainsRune(vRuleChars, char)
		if isRule && runStart < 0 {
			runStart = y
		}
		if !isRule && runStart >= 0 {
			if y-runStart >= minVerticalRun {
				rules = append(rules, Rule{Kind: "v", X: x, Y: runStart, Length: y - runStart})
			}
			runStart = -1
		}
	}
	if runStart >= 0 && grid.RowsN-runStart >= minVerticalRun {
		rules = append(rules, Rule{Kind: "v", X: x, Y: runStart, Length: grid.RowsN - runStart})
	}
	return rules
}

// pairPanels matches horizontal rules with the next rule that overlaps them into
// a rectangle. Real TUIs put titles inside the top border ("┌─ Tasks ──┐") and
// sometimes close with a plain rule, so spans rarely match exactly: overlap is
// the reliable signal, and the union span is the panel.
func pairPanels(hRules []Rule, vRules []Rule, grid *Grid) []Panel {
	panels := []Panel{}
	for index, top := range hRules {
		// Pick the best closing rule: a span that matches well and sits closest
		// below. Taking the first overlap instead pairs an outer top border with
		// an inner box and hides the real nesting depth.
		bestIndex, bestScore := -1, -1.0
		for candidateIndex, bottom := range hRules[index+1:] {
			if bottom.Y <= top.Y {
				continue
			}
			union, ok := unionSpan(top, bottom)
			if !ok {
				continue
			}
			spanDelta := math.Abs(float64(union.Length-top.Length)) + math.Abs(float64(union.Length-bottom.Length))
			distance := float64(bottom.Y - top.Y)
			score := 100 - spanDelta*2 - distance*0.2
			if score > bestScore {
				bestScore, bestIndex = score, index+1+candidateIndex
			}
		}
		if bestIndex < 0 {
			continue
		}
		{
			bottom := hRules[bestIndex]
			union, ok := unionSpan(top, bottom)
			if !ok {
				continue
			}
			left := edgeCoverage(grid, union.X, top.Y, bottom.Y)
			right := edgeCoverage(grid, union.X+union.Length-1, top.Y, bottom.Y)
			// A single missing border cell in a short panel is already a visible
			// defect, so the bar is high; the coverage is reported so the model
			// can weigh a one-cell gap differently from a missing wall.
			closed := left >= 0.85 && right >= 0.85
			panels = append(panels, Panel{
				Region:   Region{X0: union.X, Y0: top.Y, X1: union.X + union.Length - 1, Y1: bottom.Y},
				Closed:   closed,
				Coverage: minFloat64(left, right),
			})
			break
		}
	}
	return panels
}

// unionSpan merges two rules into the span that contains both, requiring enough
// overlap that they plausibly belong to the same box.
func unionSpan(a, b Rule) (Rule, bool) {
	aStart, aEnd := a.X, a.X+a.Length
	bStart, bEnd := b.X, b.X+b.Length
	overlap := minInt(aEnd, bEnd) - maxInt(aStart, bStart)
	shorter := minInt(aEnd-aStart, bEnd-bStart)
	if shorter <= 0 || float64(overlap)/float64(shorter) < 0.7 {
		return Rule{}, false
	}
	start := minInt(aStart, bStart)
	end := maxInt(aEnd, bEnd)
	return Rule{Kind: "h", X: start, Y: a.Y, Length: end - start}, true
}

func edgeCoverage(grid *Grid, x, y0, y1 int) float64 {
	if y1 <= y0 {
		return 0
	}
	drawn := 0
	for y := y0; y <= y1; y++ {
		char := runeAt(grid, x, y)
		if strings.ContainsRune(vRuleChars, char) || strings.ContainsRune("┌┐└┘├┤┬┴┼╭╮╰╯╔╗╚╝", char) {
			drawn++
		}
	}
	return float64(drawn) / float64(y1-y0+1)
}

var keyHintPattern = regexp.MustCompile(
	`(?:(?P<mod>\^|C-|Ctrl\+|Alt\+|M-|S-)?(?P<key>[A-Za-z0-9]|Esc|Enter|Tab|Space|↑|↓|←|→|F\d{1,2}))` +
		`\s*[:\-–]?\s*(?P<label>[A-Z][A-Za-z0-9 ,./'()&+_-]{1,28})`,
)

func keyHints(grid *Grid) []KeyHint {
	hints := []KeyHint{}
	seen := map[string]bool{}
	for y := 0; y < grid.RowsN; y++ {
		row := grid.Row(y)
		trimmed := strings.TrimRight(row, " ")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		looksLikeBar := (strings.Count(trimmed, "  ") >= 2 && (strings.Contains(lower, "esc") ||
			strings.Contains(lower, "enter") || strings.Contains(lower, "tab") || strings.Contains(lower, "^"))) ||
			containsAny(lower, []string{"quit", "exit", "help", "save", "search", "next", "back", "select", "toggle", "open", "menu"})
		if !looksLikeBar {
			continue
		}
		for _, match := range keyHintPattern.FindAllStringSubmatchIndex(trimmed, -1) {
			keyStart, keyEnd := match[4], match[5]
			labelStart, labelEnd := match[6], match[7]
			if keyStart < 0 || keyEnd <= keyStart || labelStart < 0 || labelEnd <= labelStart {
				continue // optional groups did not participate
			}
			key := trimmed[keyStart:keyEnd]
			if modStart, modEnd := match[2], match[3]; modStart >= 0 && modEnd > modStart {
				key = trimmed[modStart:modEnd] + key
			}
			label := strings.TrimSpace(trimmed[labelStart:labelEnd])
			if len(label) < 2 {
				continue
			}
			signature := strings.ToLower(key) + "\x00" + strings.ToLower(label)
			if seen[signature] {
				continue
			}
			seen[signature] = true
			hints = append(hints, KeyHint{Key: key, Label: label, X: match[0], Y: y})
			if len(hints) >= maxKeyHints {
				return hints
			}
		}
	}
	return hints
}

func highlights(grid *Grid) []Highlight {
	out := []Highlight{}
	for y := 0; y < grid.RowsN; y++ {
		start := -1
		kind := ""
		for x := 0; x <= grid.Cols; x++ {
			cell, ok := grid.At(x, y)
			highlighted := false
			current := ""
			if ok {
				switch {
				case cell.Reverse:
					highlighted, current = true, "reverse"
				case isPaintedBackground(cell) && cell.Char != ' ':
					highlighted, current = true, "bg"
				}
			}
			if highlighted && start < 0 {
				start, kind = x, current
			} else if !highlighted && start >= 0 {
				if x-start >= minHighlightRun {
					text := strings.TrimSpace(grid.Row(y)[start:x])
					out = append(out, Highlight{
						Region: Region{X0: start, Y0: y, X1: x - 1, Y1: y},
						Kind:   kind,
						Text:   text,
					})
					if len(out) >= maxHighlights {
						return out
					}
				}
				start = -1
			}
		}
	}
	return out
}

func isPaintedBackground(cell Cell) bool {
	return !IsDefaultBG(cell.BG)
}

var percentPattern = regexp.MustCompile(`(\d{1,3})\s*%`)

func bars(grid *Grid) []Bar {
	out := []Bar{}
	for y := 0; y < grid.RowsN; y++ {
		row := grid.Row(y)
		start := -1
		filled := 0.0
		for x := 0; x <= grid.Cols; x++ {
			char := rune(0)
			if x < grid.Cols {
				char = runeAt(grid, x, y)
			}
			isBar := strings.ContainsRune(barChars, char)
			if isBar {
				if start < 0 {
					start = x
				}
				filled += barWeight(char)
				continue
			}
			if start >= 0 {
				length := x - start
				if length >= minBarLength {
					bar := Bar{
						Region:   Region{X0: start, Y0: y, X1: x - 1, Y1: y},
						Fraction: clampFloat(filled/float64(length), 0, 1),
						Label:    strings.TrimSpace(row),
					}
					if match := percentPattern.FindStringSubmatch(row); match != nil {
						value := atoiSafe(match[1])
						if value >= 0 && value <= 100 {
							bar.Percent = &value
						}
					}
					out = append(out, bar)
				}
				start = -1
				filled = 0
			}
		}
	}
	return out
}

func barWeight(char rune) float64 {
	switch char {
	case '█', '▉':
		return 1
	case '▓':
		return 0.75
	case '▒':
		return 0.5
	case '░':
		return 0.25
	case '▊':
		return 0.85
	case '▋':
		return 0.7
	case '▌':
		return 0.5
	case '▍':
		return 0.35
	case '▎':
		return 0.2
	case '▏':
		return 0.1
	default:
		return 0
	}
}

func spinners(grid *Grid) []Spinner {
	out := []Spinner{}
	for y := 0; y < grid.RowsN; y++ {
		for x := 0; x < grid.Cols; x++ {
			char := runeAt(grid, x, y)
			if isSpinnerRune(char) {
				out = append(out, Spinner{X: x, Y: y, Char: string(char)})
			}
		}
	}
	return out
}

func isSpinnerRune(char rune) bool {
	if char >= 0x2800 && char <= 0x28FF { // braille patterns
		return true
	}
	switch char {
	case '⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏', '◐', '◓', '◑', '◒', '⣾', '⣽', '⣻', '⢿', '⡿', '⣟', '⣯', '⣷':
		return true
	default:
		return false
	}
}

func textRegions(grid *Grid) []TextRegion {
	regions := []TextRegion{}
	start := -1
	for y := 0; y <= grid.RowsN; y++ {
		filled := y < grid.RowsN && strings.TrimSpace(grid.Row(y)) != ""
		if filled && start < 0 {
			start = y
			continue
		}
		if !filled && start >= 0 {
			x0, x1 := grid.Cols, 0
			for row := start; row < y; row++ {
				text := grid.Row(row)
				left := len(text) - len(strings.TrimLeft(text, " "))
				right := len(strings.TrimRight(text, " "))
				if left < x0 {
					x0 = left
				}
				if right-1 > x1 {
					x1 = right - 1
				}
			}
			regions = append(regions, TextRegion{
				Region: Region{X0: x0, Y0: start, X1: maxInt(x1, x0), Y1: y - 1},
				Lines:  y - start,
			})
			start = -1
		}
	}
	sort.SliceStable(regions, func(i, j int) bool {
		return regions[i].Lines > regions[j].Lines
	})
	if len(regions) > maxTextRegions {
		regions = regions[:maxTextRegions]
	}
	return regions
}

// SummaryForModel renders a compact, token-conscious description of the screen
// for the decision tier: the visible text plus the structure that a human would
// notice at a glance.
func (g *Grid) SummaryForModel() string {
	var builder strings.Builder
	structure := g.Structure()
	fmt.Fprintf(&builder, "terminal: %dx%d, cursor=(%d,%d) visible=%t\n",
		g.Cols, g.RowsN, g.CursorX, g.CursorY, g.CursorVisible)
	text := g.TrimmedText()
	lines := strings.Split(text, "\n")
	if len(lines) > maxRowsInSummary {
		lines = append(lines[:maxRowsInSummary], fmt.Sprintf("... %d more rows", len(lines)-maxRowsInSummary))
	}
	builder.WriteString("rows (numbered from 1):\n")
	for index, line := range lines {
		fmt.Fprintf(&builder, "%3d|%s\n", index+1, line)
	}
	if len(structure.Panels) > 0 {
		builder.WriteString("panels:\n")
		for _, panel := range structure.Panels {
			state := "edges missing"
			if panel.Closed {
				state = "closed"
			}
			fmt.Fprintf(&builder, "- %s (%s)\n", panel.Region, state)
		}
	}
	if len(structure.KeyHints) > 0 {
		builder.WriteString("advertised keys:\n")
		for _, hint := range structure.KeyHints {
			fmt.Fprintf(&builder, "- %s -> %q\n", hint.Key, hint.Label)
		}
	}
	if len(structure.Highlights) > 0 {
		builder.WriteString("highlighted/selected runs:\n")
		for _, highlight := range structure.Highlights {
			fmt.Fprintf(&builder, "- row %d %s %q\n", highlight.Y0, highlight.Region, highlight.Text)
		}
	}
	if len(structure.Bars) > 0 {
		builder.WriteString("progress bars:\n")
		for _, bar := range structure.Bars {
			percent := ""
			if bar.Percent != nil {
				percent = fmt.Sprintf(", label says %d%%", *bar.Percent)
			}
			fmt.Fprintf(&builder, "- row %d %s filled %.2f%s\n", bar.Y0, bar.Region, bar.Fraction, percent)
		}
	}
	if len(structure.Spinners) > 0 {
		builder.WriteString("spinner glyphs: ")
		parts := make([]string, 0, len(structure.Spinners))
		for _, spinner := range structure.Spinners {
			parts = append(parts, fmt.Sprintf("%q@%d,%d", spinner.Char, spinner.X, spinner.Y))
		}
		builder.WriteString(strings.Join(parts, " "))
		builder.WriteString("\n")
	}
	return builder.String()
}

func runeAt(grid *Grid, x, y int) rune {
	if y < 0 || y >= grid.RowsN || x < 0 || x >= grid.Cols {
		return ' '
	}
	return grid.Cells[y][x].Char
}

func containsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func atoiSafe(raw string) int {
	value := 0
	for _, char := range raw {
		if !unicode.IsDigit(char) {
			return -1
		}
		value = value*10 + int(char-'0')
		if value > 1000 {
			return -1
		}
	}
	return value
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clampFloat(value, low, high float64) float64 {
	switch {
	case value < low:
		return low
	case value > high:
		return high
	default:
		return value
	}
}

func minFloat64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

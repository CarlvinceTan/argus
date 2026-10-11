package screen

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"regexp"
	"strings"
	"unicode"
)

// Annotation marks a rectangular area on a rendered still.
type Annotation struct {
	Region  Region
	Label   string
	Colour  color.RGBA
	Filled  bool
	Opacity uint8
}

// AnnotationPalette maps a severity or category to an accent colour.
var AnnotationPalette = map[string]color.RGBA{
	"critical": {R: 0xff, G: 0x4d, B: 0x4d, A: 0xff},
	"major":    {R: 0xff, G: 0x9f, B: 0x1c, A: 0xff},
	"minor":    {R: 0xff, G: 0xd6, B: 0x0a, A: 0xff},
	"info":     {R: 0x6c, G: 0x9a, B: 0xff, A: 0xff},
	"cursor":   {R: 0xa0, G: 0xa8, B: 0xff, A: 0xff},
}

// RenderAnnotated draws a grid with rectangles around the areas of interest.
// This is the still that both the report and the visualiser show: the reviewer
// sees the screen exactly as the subject drew it, with the suspect region boxed.
func RenderAnnotated(grid *Grid, annotations []Annotation, opts RenderOptions) ([]byte, error) {
	raw, err := RenderPNG(grid, opts)
	if err != nil {
		return nil, err
	}
	if len(annotations) == 0 {
		return raw, nil
	}
	canvas, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decoding render: %w", err)
	}
	_, charW, charH, _, err := loadFace(opts.FontSize, opts.DPI)
	if err != nil {
		return nil, err
	}
	target := image.NewRGBA(canvas.Bounds())
	draw.Draw(target, target.Bounds(), canvas, canvas.Bounds().Min, draw.Src)
	for _, annotation := range annotations {
		region := annotation.Region
		rect := image.Rect(
			region.X0*charW,
			region.Y0*charH,
			(region.X1+1)*charW,
			(region.Y1+1)*charH,
		)
		accent := annotation.Colour
		if accent.A == 0 {
			accent = AnnotationPalette["major"]
		}
		if annotation.Filled {
			alpha := annotation.Opacity
			if alpha == 0 {
				alpha = 0x30
			}
			fill := color.RGBA{R: accent.R, G: accent.G, B: accent.B, A: alpha}
			draw.Draw(target, rect, &image.Uniform{C: fill}, image.Point{}, draw.Over)
		}
		thickness := 2
		draw.Draw(target, image.Rect(rect.Min.X, rect.Min.Y, rect.Max.X, rect.Min.Y+thickness), &image.Uniform{C: accent}, image.Point{}, draw.Src)
		draw.Draw(target, image.Rect(rect.Min.X, rect.Max.Y-thickness, rect.Max.X, rect.Max.Y), &image.Uniform{C: accent}, image.Point{}, draw.Src)
		draw.Draw(target, image.Rect(rect.Min.X, rect.Min.Y, rect.Min.X+thickness, rect.Max.Y), &image.Uniform{C: accent}, image.Point{}, draw.Src)
		draw.Draw(target, image.Rect(rect.Max.X-thickness, rect.Min.Y, rect.Max.X, rect.Max.Y), &image.Uniform{C: accent}, image.Point{}, draw.Src)
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, target); err != nil {
		return nil, fmt.Errorf("encoding annotated still: %w", err)
	}
	return buffer.Bytes(), nil
}

// ScreenName guesses a stable, human-meaningful label for the current screen so
// findings can be grouped by page. It takes the first phrase of the header row
// (for example "New Session" from "New Session    /private/var/folders/..."),
// which keeps the page name stable across sessions and dynamic values.
func ScreenName(grid *Grid) string {
	if grid == nil {
		return "unknown"
	}
	for y := 0; y < minInt(grid.RowsN, 3); y++ {
		if name := headerPhrase(grid.Row(y)); name != "" {
			return name
		}
	}
	for _, panel := range grid.Structure().Panels {
		if title := panelTitle(grid, panel); title != "" {
			return truncateRunes(title, 40)
		}
	}
	for y := 0; y < grid.RowsN; y++ {
		if name := headerPhrase(grid.Row(y)); name != "" {
			return name
		}
	}
	return "unknown"
}

var phraseSplitter = regexp.MustCompile(`\s{2,}`)

// headerPhrase extracts the first meaningful phrase from a header-like row.
func headerPhrase(row string) string {
	text := strings.TrimSpace(strings.Trim(strings.Trim(row, "│┃║| "), "─━═-"))
	if text == "" || isRuleRow(text) || looksLikeKeyBar(text) {
		return ""
	}
	for _, part := range phraseSplitter.Split(text, -1) {
		cleaned := strings.TrimSpace(strings.Trim(part, "….:|/-–—?!"))
		if len([]rune(cleaned)) < 3 || !containsLetter(cleaned) {
			continue
		}
		if looksLikeKeyBar(cleaned) || looksLikeValue(cleaned) {
			continue
		}
		return truncateRunes(cleaned, 40)
	}
	return ""
}

// looksLikeValue rejects phrases that are data rather than a page name: paths,
// hashes, numbers, timestamps and single tokens. A page name should be words.
func looksLikeValue(text string) bool {
	if strings.ContainsAny(text, "/\\") && !strings.Contains(text, " ") {
		return true
	}
	if strings.HasPrefix(text, "~") || strings.HasPrefix(text, ".") {
		return true
	}
	letters, digits, spaces := 0, 0, 0
	for _, char := range text {
		switch {
		case unicode.IsLetter(char):
			letters++
		case unicode.IsDigit(char):
			digits++
		case char == ' ':
			spaces++
		}
	}
	if digits > letters {
		return true
	}
	if spaces == 0 && letters <= 4 {
		return true
	}
	return false
}

func containsLetter(text string) bool {
	for _, char := range text {
		if unicode.IsLetter(char) {
			return true
		}
	}
	return false
}

// FeatureLabels lists the visible feature names on a screen: panel titles and
// highlighted rows. These are the grouping keys the report and the visualiser
// use to build "issues by page/feature" views.
func FeatureLabels(grid *Grid) []string {
	labels := []string{}
	seen := map[string]bool{}
	add := func(text string) {
		cleaned := strings.TrimSpace(strings.Trim(text, "─━═-│┃║| "))
		cleaned = strings.TrimSpace(cleaned)
		if len(cleaned) < 4 || len(cleaned) > 48 {
			return
		}
		if looksLikeKeyBar(cleaned) {
			return
		}
		if !strings.ContainsFunc(cleaned, unicode.IsLower) && !strings.ContainsAny(cleaned, " ") {
			// Tokens such as "REC" or "ERR" are state badges, not feature names.
			return
		}
		key := strings.ToLower(cleaned)
		if seen[key] {
			return
		}
		seen[key] = true
		labels = append(labels, cleaned)
	}
	structure := grid.Structure()
	for _, panel := range structure.Panels {
		add(panelTitle(grid, panel))
	}
	for _, highlight := range structure.Highlights {
		add(highlight.Text)
	}
	for _, hint := range structure.KeyHints {
		add(hint.Label)
	}
	if len(labels) > 8 {
		labels = labels[:8]
	}
	return labels
}

// looksLikeKeyBar filters out footer hint bars, which are neither a page nor a
// feature and would otherwise become a grouping key.
func looksLikeKeyBar(text string) bool {
	lower := strings.ToLower(text)
	markers := 0
	for _, marker := range []string{"quit", "help", "enter", "esc", "tab", "^", "toggle", "select", "save", "search"} {
		if strings.Contains(lower, marker) {
			markers++
		}
	}
	return markers >= 2
}

// panelTitle extracts the text inside a panel's top border, e.g. " Sessions ".
func panelTitle(grid *Grid, panel Panel) string {
	if panel.Y0 < 0 || panel.Y0 >= grid.RowsN {
		return ""
	}
	row := grid.Row(panel.Y0)
	if panel.X1+1 > len([]rune(row)) {
		return ""
	}
	runes := []rune(row)
	if panel.X0 >= len(runes) {
		return ""
	}
	segment := string(runes[panel.X0 : panel.X1+1])
	trimmed := strings.Trim(segment, "─━═-┌┐└┘├┤┬┴┼╭╮╰╯ ")
	return strings.TrimSpace(trimmed)
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return strings.TrimSpace(string(runes[:limit]))
}

// IsMostlyText reports whether a region looks like prose (used when deciding
// whether to send a crop or the whole screen to the vision tier).
func IsMostlyText(grid *Grid, region Region) bool {
	total, alpha := 0, 0
	for y := region.Y0; y <= region.Y1; y++ {
		for x := region.X0; x <= region.X1; x++ {
			cell, ok := grid.At(x, y)
			if !ok || cell.Char == ' ' {
				continue
			}
			total++
			if unicode.IsLetter(cell.Char) || unicode.IsDigit(cell.Char) {
				alpha++
			}
		}
	}
	return total > 0 && float64(alpha)/float64(total) > 0.5
}

const (
	boxRunes    = "─│┌┐└┘├┤┬┴┼━┃┏┓┗┛╔╗╚╝╭╮╰╯═║╒╓╕╖╘╙╛╜╞╟╠╡╢╣╤╥╦╧╨╩╪╫╬▏▕┄┅┆┇┊┋╌╍"
	cornerRunes = "┌┐└┘├┤┬┴┼╭╮╰╯╔╗╚╝╠╣╦╩╬╞╟╡╢"
)

// isRuleRow reports whether a row consists only of box-drawing characters.
func isRuleRow(text string) bool {
	rules, other := 0, 0
	for _, char := range text {
		switch {
		case char == ' ':
		case strings.ContainsRune(boxRunes+cornerRunes, char):
			rules++
		default:
			other++
		}
	}
	return rules > 0 && other == 0
}

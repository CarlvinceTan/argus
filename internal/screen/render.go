package screen

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"sync"

	"github.com/hinshun/vt10x"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// FontPaths are tried in order when loading a monospace face.
var FontPaths = []string{
	"/System/Library/Fonts/SFNSMono.ttf",
	"/System/Library/Fonts/Supplemental/Andale Mono.ttf",
	"/System/Library/Fonts/Supplemental/Courier New.ttf",
	"/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
}

// Theme maps the terminal defaults and the xterm-256 palette to pixels. Fonts
// and themes differ between machines, so findings that depend on colour are
// phrased in terms of the palette index as well as the rendered pixel.
type Theme struct {
	Foreground color.RGBA
	Background color.RGBA
	Palette    [256]color.RGBA
}

// DefaultTheme is a dark terminal theme, matching the environment Argus runs in.
func DefaultTheme() Theme {
	theme := Theme{
		Foreground: color.RGBA{R: 0xd8, G: 0xd8, B: 0xd8, A: 0xff},
		Background: color.RGBA{R: 0x12, G: 0x12, B: 0x14, A: 0xff},
	}
	base := [16][3]uint8{
		{0x00, 0x00, 0x00}, {0xcd, 0x31, 0x31}, {0x0d, 0xbc, 0x79}, {0xe5, 0xc0, 0x7b},
		{0x24, 0x72, 0xc8}, {0xbc, 0x3f, 0xbc}, {0x11, 0xa8, 0xcd}, {0xe5, 0xe5, 0xe5},
		{0x66, 0x66, 0x66}, {0xef, 0x29, 0x29}, {0x1a, 0xf7, 0x8c}, {0xfc, 0xdd, 0x6a},
		{0x3b, 0x8e, 0xea}, {0xf7, 0x8c, 0xf7}, {0x14, 0xd0, 0xf7}, {0xff, 0xff, 0xff},
	}
	for index := 0; index < 16; index++ {
		theme.Palette[index] = color.RGBA{R: base[index][0], G: base[index][1], B: base[index][2], A: 0xff}
	}
	steps := [6]uint8{0, 0x5f, 0x87, 0xaf, 0xd7, 0xff}
	for r := 0; r < 6; r++ {
		for g := 0; g < 6; g++ {
			for b := 0; b < 6; b++ {
				index := 16 + 36*r + 6*g + b
				theme.Palette[index] = color.RGBA{R: steps[r], G: steps[g], B: steps[b], A: 0xff}
			}
		}
	}
	for index := 0; index < 24; index++ {
		level := uint8(8 + index*10)
		theme.Palette[232+index] = color.RGBA{R: level, G: level, B: level, A: 0xff}
	}
	return theme
}

// Resolve maps a terminal colour to an RGBA value.
func (t Theme) Resolve(value vt10x.Color, background bool) color.RGBA {
	switch {
	case value == vt10x.DefaultFG:
		return t.Foreground
	case value == vt10x.DefaultBG:
		return t.Background
	case value < 256:
		return t.Palette[value]
	default:
		rgb := uint32(value)
		return color.RGBA{
			R: uint8((rgb >> 16) & 0xff),
			G: uint8((rgb >> 8) & 0xff),
			B: uint8(rgb & 0xff),
			A: 0xff,
		}
	}
}

// RenderOptions controls PNG rendering.
type RenderOptions struct {
	FontSize  float64
	DPI       float64
	Theme     Theme
	Region    *Region
	Cursor    bool
	MaxWidth  int
	Separator color.RGBA
}

// DefaultRenderOptions is a readable default for evidence stills.
func DefaultRenderOptions() RenderOptions {
	return RenderOptions{
		FontSize:  13,
		DPI:       72,
		Theme:     DefaultTheme(),
		Cursor:    true,
		MaxWidth:  1600,
		Separator: color.RGBA{R: 0x50, G: 0x50, B: 0x58, A: 0xff},
	}
}

var (
	fontOnce sync.Once
	face     font.Face
	faceErr  error
	cellW    int
	cellH    int
	ascent   int
)

func loadFace(size, dpi float64) (font.Face, int, int, int, error) {
	fontOnce.Do(func() {
		var parsed *opentype.Font
		for _, path := range FontPaths {
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			parsed, err = opentype.Parse(raw)
			if err == nil {
				break
			}
		}
		if parsed == nil {
			fallback, err := opentype.Parse(gomono.TTF)
			if err != nil {
				faceErr = fmt.Errorf("no usable monospace font: %w", err)
				return
			}
			parsed = fallback
		}
		loaded, err := opentype.NewFace(parsed, &opentype.FaceOptions{
			Size:    size,
			DPI:     dpi,
			Hinting: font.HintingFull,
		})
		if err != nil {
			faceErr = fmt.Errorf("building font face: %w", err)
			return
		}
		metrics := loaded.Metrics()
		face = loaded
		ascent = metrics.Ascent.Ceil()
		cellH = (metrics.Ascent + metrics.Descent).Ceil() + 2
		advance, ok := loaded.GlyphAdvance('M')
		if !ok {
			cellW = metrics.Height.Ceil() / 2
		} else {
			cellW = advance.Ceil()
		}
		if cellW < 4 {
			cellW = 8
		}
		if cellH < 8 {
			cellH = 16
		}
	})
	return face, cellW, cellH, ascent, faceErr
}

// RenderPNG draws a grid to a PNG image.
func RenderPNG(grid *Grid, opts RenderOptions) ([]byte, error) {
	if grid == nil {
		return nil, fmt.Errorf("no grid to render")
	}
	loadedFace, charW, charH, baseline, err := loadFace(opts.FontSize, opts.DPI)
	if err != nil {
		return nil, err
	}
	source := grid
	if opts.Region != nil {
		source = grid.Crop(opts.Region.X0, opts.Region.Y0, opts.Region.X1+1, opts.Region.Y1+1)
	}
	width := source.Cols * charW
	height := source.RowsN * charH
	if opts.MaxWidth > 0 && width > opts.MaxWidth {
		// Downscale the cell size until the image fits, then reload the face.
		scale := float64(opts.MaxWidth) / float64(width)
		adjusted := opts
		adjusted.FontSize = opts.FontSize * scale
		adjusted.MaxWidth = 0
		return RenderPNG(grid, adjusted)
	}
	canvas := image.NewRGBA(image.Rect(0, 0, maxInt(width, 1), maxInt(height, 1)))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: opts.Theme.Background}, image.Point{}, draw.Src)

	drawer := &font.Drawer{Dst: canvas, Face: loadedFace}
	for y := 0; y < source.RowsN; y++ {
		for x := 0; x < source.Cols; x++ {
			cell := source.Cells[y][x]
			if cell.Char == ' ' || cell.Char == 0 {
				continue
			}
			fg := opts.Theme.Resolve(cell.FG, false)
			bg := opts.Theme.Resolve(cell.BG, true)
			if cell.Reverse {
				fg, bg = bg, fg
			}
			rect := image.Rect(x*charW, y*charH, (x+1)*charW, (y+1)*charH)
			if bg != opts.Theme.Background {
				draw.Draw(canvas, rect, &image.Uniform{C: bg}, image.Point{}, draw.Src)
			}
			drawer.Src = &image.Uniform{C: fg}
			drawer.Dot = fixed.Point26_6{
				X: fixed.I(x*charW) + fixed.I(1),
				Y: fixed.I(y*charH + baseline),
			}
			drawer.DrawString(string(cell.Char))
			if cell.Underline {
				underlineY := (y+1)*charH - 2
				draw.Draw(canvas, image.Rect(x*charW, underlineY, (x+1)*charW, underlineY+1), &image.Uniform{C: fg}, image.Point{}, draw.Src)
			}
		}
	}
	if opts.Cursor && source.CursorVisible {
		x, y := source.CursorX, source.CursorY
		if x >= 0 && x < source.Cols && y >= 0 && y < source.RowsN {
			rect := image.Rect(x*charW, y*charH, (x+1)*charW, (y+1)*charH)
			cursorColor := color.RGBA{R: 0x9a, G: 0xa0, B: 0xff, A: 0x99}
			draw.Draw(canvas, rect, &image.Uniform{C: cursorColor}, image.Point{}, draw.Over)
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, canvas); err != nil {
		return nil, fmt.Errorf("encoding png: %w", err)
	}
	return buffer.Bytes(), nil
}

// RenderStrip draws several grids side by side, left to right, with separators.
// This is the animation evidence format: flicker and layout drift are obvious
// when consecutive frames are adjacent.
func RenderStrip(grids []*Grid, opts RenderOptions) ([]byte, error) {
	if len(grids) == 0 {
		return nil, fmt.Errorf("no frames to render")
	}
	if len(grids) == 1 {
		return RenderPNG(grids[0], opts)
	}
	images := make([][]byte, 0, len(grids))
	widths := make([]int, 0, len(grids))
	height := 0
	for _, grid := range grids {
		raw, err := RenderPNG(grid, opts)
		if err != nil {
			return nil, err
		}
		images = append(images, raw)
		decoded, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("decoding intermediate frame: %w", err)
		}
		widths = append(widths, decoded.Bounds().Dx())
		if decoded.Bounds().Dy() > height {
			height = decoded.Bounds().Dy()
		}
	}
	separator := opts.Separator
	if separator.A == 0 {
		separator = color.RGBA{R: 0x50, G: 0x50, B: 0x58, A: 0xff}
	}
	total := 0
	for _, width := range widths {
		total += width
	}
	total += (len(images) - 1) * 3
	canvas := image.NewRGBA(image.Rect(0, 0, total, height))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: opts.Theme.Background}, image.Point{}, draw.Src)
	x := 0
	for index, raw := range images {
		decoded, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("decoding frame: %w", err)
		}
		draw.Draw(canvas, image.Rect(x, 0, x+widths[index], height), decoded, image.Point{}, draw.Src)
		x += widths[index]
		if index < len(images)-1 {
			draw.Draw(canvas, image.Rect(x, 0, x+3, height), &image.Uniform{C: separator}, image.Point{}, draw.Src)
			x += 3
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, canvas); err != nil {
		return nil, fmt.Errorf("encoding strip: %w", err)
	}
	return buffer.Bytes(), nil
}

// ContrastRatio computes the WCAG contrast ratio between two colours.
func ContrastRatio(a, b color.RGBA) float64 {
	la := relativeLuminance(a)
	lb := relativeLuminance(b)
	high, low := la, lb
	if high < low {
		high, low = low, high
	}
	return (high + 0.05) / (low + 0.05)
}

func relativeLuminance(c color.RGBA) float64 {
	channel := func(value uint8) float64 {
		normalized := float64(value) / 255
		if normalized <= 0.03928 {
			return normalized / 12.92
		}
		return powApprox((normalized+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(c.R) + 0.7152*channel(c.G) + 0.0722*channel(c.B)
}

func powApprox(base, exponent float64) float64 {
	result := 1.0
	term := 1.0
	for n := 1; n <= 24; n++ {
		term *= (exponent - float64(n) + 1) / float64(n) * (base - 1)
		result += term
	}
	return result
}

// RenderStripFromPNG lays pre-rendered PNGs out side by side with separators.
func RenderStripFromPNG(images [][]byte, opts RenderOptions) ([]byte, error) {
	if len(images) == 0 {
		return nil, fmt.Errorf("no images to render")
	}
	if len(images) == 1 {
		return images[0], nil
	}
	widths := make([]int, 0, len(images))
	height := 0
	for _, raw := range images {
		decoded, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("decoding frame: %w", err)
		}
		widths = append(widths, decoded.Bounds().Dx())
		if decoded.Bounds().Dy() > height {
			height = decoded.Bounds().Dy()
		}
	}
	separator := opts.Separator
	if separator.A == 0 {
		separator = color.RGBA{R: 0x50, G: 0x50, B: 0x58, A: 0xff}
	}
	total := (len(images) - 1) * 3
	for _, width := range widths {
		total += width
	}
	canvas := image.NewRGBA(image.Rect(0, 0, total, height))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: opts.Theme.Background}, image.Point{}, draw.Src)
	x := 0
	for index, raw := range images {
		decoded, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("decoding frame: %w", err)
		}
		draw.Draw(canvas, image.Rect(x, 0, x+widths[index], height), decoded, decoded.Bounds().Min, draw.Src)
		x += widths[index]
		if index < len(images)-1 {
			draw.Draw(canvas, image.Rect(x, 0, x+3, height), &image.Uniform{C: separator}, image.Point{}, draw.Src)
			x += 3
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, canvas); err != nil {
		return nil, fmt.Errorf("encoding strip: %w", err)
	}
	return buffer.Bytes(), nil
}

// RenderAnnotatedStrip renders several frames with the same highlight box on
// each, which is how animation defects are shown to a reviewer.
func RenderAnnotatedStrip(grids []*Grid, annotations []Annotation, opts RenderOptions) ([]byte, error) {
	images := make([][]byte, 0, len(grids))
	for _, grid := range grids {
		rendered, err := RenderAnnotated(grid, annotations, opts)
		if err != nil {
			return nil, err
		}
		images = append(images, rendered)
	}
	return RenderStripFromPNG(images, opts)
}

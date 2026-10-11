package screen

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// TileGrid is the coarse grounding scheme used for GUI findings: the screenshot
// is overlaid with labelled tiles, the vision tier names the tile it is talking
// about, and code turns that label back into pixel coordinates. Grounding is
// mechanical, so a model cannot invent a region that does not exist.
type TileGrid struct {
	Cols, Rows int
	Width      int
	Height     int
}

// DefaultTileGrid is the grid used for GUI screenshots.
func DefaultTileGrid(width, height int) TileGrid {
	cols, rows := 4, 3
	if width < 400 {
		cols = 2
	}
	return TileGrid{Cols: cols, Rows: rows, Width: width, Height: height}
}

// Label returns the label for a tile index, e.g. "B2".
func (g TileGrid) Label(col, row int) string {
	if col < 0 || row < 0 || col >= g.Cols || row >= g.Rows {
		return ""
	}
	return fmt.Sprintf("%c%d", 'A'+col, row+1)
}

// Region returns the pixel region of a tile.
func (g TileGrid) Region(label string) (Region, bool) {
	if len(label) < 2 {
		return Region{}, false
	}
	col := int(label[0] - 'A')
	row := int(label[1] - '1')
	if col < 0 || row < 0 || col >= g.Cols || row >= g.Rows {
		return Region{}, false
	}
	tileWidth := g.Width / g.Cols
	tileHeight := g.Height / g.Rows
	return Region{
		X0: col * tileWidth,
		Y0: row * tileHeight,
		X1: minInt((col+1)*tileWidth-1, g.Width-1),
		Y1: minInt((row+1)*tileHeight-1, g.Height-1),
	}, true
}

// RegionTiles lists every tile region in order.
func (g TileGrid) RegionTiles() []Region {
	out := make([]Region, 0, g.Cols*g.Rows)
	for row := 0; row < g.Rows; row++ {
		for col := 0; col < g.Cols; col++ {
			region, _ := g.Region(g.Label(col, row))
			out = append(out, region)
		}
	}
	return out
}

// Describe returns a prompt fragment listing the tiles.
func (g TileGrid) Describe() string {
	out := ""
	for row := 0; row < g.Rows; row++ {
		for col := 0; col < g.Cols; col++ {
			if out != "" {
				out += ", "
			}
			out += g.Label(col, row)
		}
	}
	return out
}

// OverlayTiles draws the tile grid and its labels onto a copy of the image, so
// the vision tier can refer to tiles by name.
func OverlayTiles(img image.Image, grid TileGrid) *image.RGBA {
	bounds := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(out, out.Bounds(), img, bounds.Min, draw.Src)
	line := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0x44}
	tileWidth := out.Bounds().Dx() / grid.Cols
	tileHeight := out.Bounds().Dy() / grid.Rows
	for col := 1; col < grid.Cols; col++ {
		x := col * tileWidth
		draw.Draw(out, image.Rect(x, 0, x+1, out.Bounds().Dy()), &image.Uniform{C: line}, image.Point{}, draw.Src)
	}
	for row := 1; row < grid.Rows; row++ {
		y := row * tileHeight
		draw.Draw(out, image.Rect(0, y, out.Bounds().Dx(), y+1), &image.Uniform{C: line}, image.Point{}, draw.Src)
	}
	for row := 0; row < grid.Rows; row++ {
		for col := 0; col < grid.Cols; col++ {
			label := grid.Label(col, row)
			drawer := &font.Drawer{
				Dst:  out,
				Src:  &image.Uniform{C: color.RGBA{R: 0xff, G: 0xd6, B: 0x0a, A: 0xff}},
				Face: basicfont.Face7x13,
				Dot:  fixed.P(col*tileWidth+3, row*tileHeight+13),
			}
			drawer.DrawString(label)
		}
	}
	return out
}

// AnnotateImage draws a highlight box around a pixel region.
func AnnotateImage(img image.Image, region Region, accent color.RGBA) *image.RGBA {
	bounds := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(out, out.Bounds(), img, bounds.Min, draw.Src)
	if accent.A == 0 {
		accent = AnnotationPalette["major"]
	}
	rect := image.Rect(region.X0, region.Y0, region.X1+1, region.Y1+1).Intersect(out.Bounds())
	if rect.Empty() {
		return out
	}
	fill := color.RGBA{R: accent.R, G: accent.G, B: accent.B, A: 0x28}
	draw.Draw(out, rect, &image.Uniform{C: fill}, image.Point{}, draw.Over)
	thickness := 2
	draw.Draw(out, image.Rect(rect.Min.X, rect.Min.Y, rect.Max.X, rect.Min.Y+thickness), &image.Uniform{C: accent}, image.Point{}, draw.Src)
	draw.Draw(out, image.Rect(rect.Min.X, rect.Max.Y-thickness, rect.Max.X, rect.Max.Y), &image.Uniform{C: accent}, image.Point{}, draw.Src)
	draw.Draw(out, image.Rect(rect.Min.X, rect.Min.Y, rect.Min.X+thickness, rect.Max.Y), &image.Uniform{C: accent}, image.Point{}, draw.Src)
	draw.Draw(out, image.Rect(rect.Max.X-thickness, rect.Min.Y, rect.Max.X, rect.Max.Y), &image.Uniform{C: accent}, image.Point{}, draw.Src)
	return out
}

// EncodeImagePNG encodes an image.
func EncodeImagePNG(img image.Image) []byte {
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		return nil
	}
	return buffer.Bytes()
}

// CombineImages lays images out side by side with separators (temporal evidence
// for a GUI finding).
func CombineImages(images []image.Image, separator color.RGBA) []byte {
	if len(images) == 0 {
		return nil
	}
	if separator.A == 0 {
		separator = color.RGBA{R: 0x50, G: 0x50, B: 0x58, A: 0xff}
	}
	width, height := 0, 0
	for _, img := range images {
		width += img.Bounds().Dx()
		if img.Bounds().Dy() > height {
			height = img.Bounds().Dy()
		}
	}
	width += (len(images) - 1) * 3
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.RGBA{R: 0x0b, G: 0x0c, B: 0x10, A: 0xff}}, image.Point{}, draw.Src)
	x := 0
	for index, img := range images {
		draw.Draw(canvas, image.Rect(x, 0, x+img.Bounds().Dx(), img.Bounds().Dy()), img, img.Bounds().Min, draw.Src)
		x += img.Bounds().Dx()
		if index < len(images)-1 {
			draw.Draw(canvas, image.Rect(x, 0, x+3, height), &image.Uniform{C: separator}, image.Point{}, draw.Src)
			x += 3
		}
	}
	return EncodeImagePNG(canvas)
}

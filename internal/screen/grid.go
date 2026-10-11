// Package screen is the terminal world model.
//
// A Grid is an immutable snapshot of a virtual terminal: characters, colours and
// attributes, plus derived structure (panels, key hints, bars, highlights).
// Every downstream consumer -- deterministic checks, the Jev state, PNG
// rendering -- reads this one representation, so each finding maps back to
// screen coordinates a human can verify.
package screen

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/hinshun/vt10x"
)

// Cell is one terminal cell with its attributes.
type Cell struct {
	Char      rune
	FG        vt10x.Color
	BG        vt10x.Color
	Bold      bool
	Underline bool
	Italic    bool
	Reverse   bool
}

// Same reports whether two cells render identically.
func (c Cell) Same(other Cell) bool {
	return c == other
}

// Grid is a screen snapshot. Fields are exported for JSON-friendly reporting;
// Structure is derived lazily and cached.
type Grid struct {
	Rows          []string      `json:"rows"`
	Cells         [][]Cell      `json:"-"`
	Cols          int           `json:"cols"`
	RowsN         int           `json:"row_count"`
	CursorX       int           `json:"cursor_x"`
	CursorY       int           `json:"cursor_y"`
	CursorVisible bool          `json:"cursor_visible"`
	Time          time.Duration `json:"t_ms"`
	ID            int           `json:"id"`
	Digest        string        `json:"digest"`

	structure *Structure
}

// Capture copies the current terminal state into a Grid.
func Capture(view vt10x.View, at time.Duration, id int) *Grid {
	view.Lock()
	defer view.Unlock()
	cols, rows := view.Size()
	grid := &Grid{
		Rows:  make([]string, rows),
		Cells: make([][]Cell, rows),
		Cols:  cols,
		RowsN: rows,
		Time:  at,
		ID:    id,
	}
	for y := 0; y < rows; y++ {
		line := make([]Cell, cols)
		var builder strings.Builder
		builder.Grow(cols)
		for x := 0; x < cols; x++ {
			glyph := view.Cell(x, y)
			char := glyph.Char
			if char == 0 {
				char = ' '
			}
			line[x] = Cell{
				Char:      char,
				FG:        glyph.FG,
				BG:        glyph.BG,
				Bold:      glyph.Mode&attrBold != 0,
				Underline: glyph.Mode&attrUnderline != 0,
				Italic:    glyph.Mode&attrItalic != 0,
				Reverse:   glyph.Mode&attrReverse != 0,
			}
			builder.WriteRune(char)
		}
		grid.Rows[y] = builder.String()
		grid.Cells[y] = line
	}
	cursor := view.Cursor()
	grid.CursorX, grid.CursorY = cursor.X, cursor.Y
	grid.CursorVisible = view.CursorVisible()
	grid.Digest = digestRows(grid.Rows)
	grid.structure = nil
	return grid
}

// vt10x glyph attribute bits, which are unexported in the dependency.
const (
	attrReverse = 1 << iota
	attrUnderline
	attrBold
	attrItalic
	attrBlink
)

// Text returns the screen as newline-joined rows with trailing blanks kept.
func (g *Grid) Text() string {
	return strings.Join(g.Rows, "\n")
}

// TrimmedText drops trailing spaces and trailing blank rows.
func (g *Grid) TrimmedText() string {
	lines := make([]string, 0, len(g.Rows))
	for _, row := range g.Rows {
		lines = append(lines, strings.TrimRight(row, " "))
	}
	for len(lines) > 1 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// Row returns one row of text.
func (g *Grid) Row(y int) string {
	if y < 0 || y >= g.RowsN {
		return ""
	}
	return g.Rows[y]
}

// At returns the cell at (x, y).
func (g *Grid) At(x, y int) (Cell, bool) {
	if y < 0 || y >= g.RowsN || x < 0 || x >= g.Cols {
		return Cell{}, false
	}
	return g.Cells[y][x], true
}

// NonSpace counts visible cells.
func (g *Grid) NonSpace() int {
	count := 0
	for _, row := range g.Rows {
		for _, char := range row {
			if char != ' ' {
				count++
			}
		}
	}
	return count
}

// Changed counts cells whose rendered appearance differs from another grid.
func (g *Grid) Changed(other *Grid) int {
	if other == nil || other.Cols != g.Cols || other.RowsN != g.RowsN {
		return g.Cols * g.RowsN
	}
	changed := 0
	for y := 0; y < g.RowsN; y++ {
		if g.Rows[y] == other.Rows[y] {
			continue
		}
		for x := 0; x < g.Cols; x++ {
			if !g.Cells[y][x].Same(other.Cells[y][x]) {
				changed++
			}
		}
	}
	return changed
}

// Crop returns a sub-grid. Coordinates are clamped to the grid.
func (g *Grid) Crop(x0, y0, x1, y1 int) *Grid {
	x0 = clampInt(x0, 0, g.Cols)
	x1 = clampInt(x1, x0, g.Cols)
	y0 = clampInt(y0, 0, g.RowsN)
	y1 = clampInt(y1, y0, g.RowsN)
	out := &Grid{
		Rows:  make([]string, 0, y1-y0),
		Cells: make([][]Cell, 0, y1-y0),
		Cols:  x1 - x0,
		RowsN: y1 - y0,
		Time:  g.Time,
		ID:    g.ID,
	}
	out.CursorX = clampInt(g.CursorX-x0, 0, out.Cols)
	out.CursorY = clampInt(g.CursorY-y0, 0, out.RowsN)
	out.CursorVisible = g.CursorVisible
	for y := y0; y < y1; y++ {
		out.Rows = append(out.Rows, g.Rows[y][x0:x1])
		row := make([]Cell, x1-x0)
		copy(row, g.Cells[y][x0:x1])
		out.Cells = append(out.Cells, row)
	}
	out.Digest = digestRows(out.Rows)
	return out
}

// Structure returns derived structure, computing it once.
func (g *Grid) Structure() *Structure {
	if g.structure == nil {
		g.structure = DeriveStructure(g)
	}
	return g.structure
}

func digestRows(rows []string) string {
	sum := sha256.Sum256([]byte(strings.Join(rows, "\n")))
	return hex.EncodeToString(sum[:8])
}

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// Region is a rectangular area of the screen.
type Region struct {
	X0 int `json:"x0"`
	Y0 int `json:"y0"`
	X1 int `json:"x1"`
	Y1 int `json:"y1"`
}

// String renders a region as "x0,y0-x1,y1".
func (r Region) String() string {
	return fmt.Sprintf("%d,%d-%d,%d", r.X0, r.Y0, r.X1, r.Y1)
}

// Contains reports whether a point is inside the region (inclusive).
func (r Region) Contains(x, y int) bool {
	return x >= r.X0 && x <= r.X1 && y >= r.Y0 && y <= r.Y1
}

// Width returns the region width in cells.
func (r Region) Width() int { return r.X1 - r.X0 + 1 }

// Height returns the region height in cells.
func (r Region) Height() int { return r.Y1 - r.Y0 + 1 }

// IsDefaultFG reports whether a colour is the terminal's default foreground.
func IsDefaultFG(color vt10x.Color) bool { return color == vt10x.DefaultFG }

// IsDefaultBG reports whether a colour is the terminal's default background.
func IsDefaultBG(color vt10x.Color) bool { return color == vt10x.DefaultBG }

// NewGrid builds a grid from plain rows with default attributes. It is used for
// synthetic screens in tests and for replaying saved screens.
func NewGrid(rows []string) *Grid {
	cols := 0
	for _, row := range rows {
		if len([]rune(row)) > cols {
			cols = len([]rune(row))
		}
	}
	grid := &Grid{
		Rows:  make([]string, len(rows)),
		Cells: make([][]Cell, len(rows)),
		Cols:  cols,
		RowsN: len(rows),
	}
	for y, row := range rows {
		runes := []rune(row)
		padded := make([]rune, cols)
		for x := 0; x < cols; x++ {
			if x < len(runes) {
				padded[x] = runes[x]
				continue
			}
			padded[x] = ' '
		}
		grid.Rows[y] = string(padded)
		line := make([]Cell, cols)
		for x, char := range padded {
			line[x] = Cell{Char: char, FG: vt10x.DefaultFG, BG: vt10x.DefaultBG}
		}
		grid.Cells[y] = line
	}
	grid.Digest = digestRows(grid.Rows)
	return grid
}

// WithCell returns a copy of the grid with one cell replaced. Grids are treated
// as immutable snapshots; this keeps that invariant while letting callers build
// synthetic screens.
func (g *Grid) WithCell(x, y int, cell Cell) *Grid {
	if y < 0 || y >= g.RowsN || x < 0 || x >= g.Cols {
		return g
	}
	out := &Grid{
		Rows:          append([]string{}, g.Rows...),
		Cells:         make([][]Cell, len(g.Cells)),
		Cols:          g.Cols,
		RowsN:         g.RowsN,
		CursorX:       g.CursorX,
		CursorY:       g.CursorY,
		CursorVisible: g.CursorVisible,
		Time:          g.Time,
		ID:            g.ID,
	}
	for index, row := range g.Cells {
		out.Cells[index] = append([]Cell{}, row...)
	}
	out.Cells[y][x] = cell
	runes := []rune(out.Rows[y])
	runes[x] = cell.Char
	out.Rows[y] = string(runes)
	out.Digest = digestRows(out.Rows)
	return out
}

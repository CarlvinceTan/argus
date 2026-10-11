package screen

import (
	"sort"
	"strings"
	"unicode"
)

// BoxRectangles finds rectangles drawn with box-drawing corners.
//
// Nesting, panel structure and border integrity all need this, and two
// implementations of it drifted apart once already, so it lives with the grid.
// A rectangle is a '┌' with a matching '┐' on its row and a '└' / '┘' pair below
// it; titles written into the top border are allowed.
func BoxRectangles(grid *Grid) []Region {
	out := []Region{}
	for y := 0; y < grid.RowsN; y++ {
		for x := 0; x < grid.Cols; x++ {
			if charAt(grid, x, y) != '┌' {
				continue
			}
			topRight := -1
			for scan := x + 1; scan < grid.Cols; scan++ {
				char := charAt(grid, scan, y)
				if char == '┐' {
					topRight = scan
					break
				}
				if !borderFiller(char) {
					break
				}
			}
			if topRight < 0 {
				continue
			}
			bottom := -1
			for scan := y + 1; scan < grid.RowsN; scan++ {
				if charAt(grid, x, scan) == '└' && charAt(grid, topRight, scan) == '┘' {
					bottom = scan
					break
				}
			}
			if bottom < 0 || bottom-y < 2 {
				continue
			}
			out = append(out, Region{X0: x, Y0: y, X1: topRight, Y1: bottom})
		}
	}
	return out
}

// NestingChain returns the chain of boxes nested from outermost to innermost:
// the boxes the user has to read through to reach the content.
func NestingChain(grid *Grid) []Region {
	rectangles := BoxRectangles(grid)
	if len(rectangles) < 2 {
		return nil
	}
	sort.SliceStable(rectangles, func(a, b int) bool {
		return area(rectangles[a]) > area(rectangles[b])
	})
	chain := []Region{}
	for _, candidate := range rectangles {
		if len(chain) == 0 {
			chain = append(chain, candidate)
			continue
		}
		last := chain[len(chain)-1]
		if last.Contains(candidate.X0, candidate.Y0) && last.Contains(candidate.X1, candidate.Y1) &&
			candidate.Width() < last.Width() && candidate.Height() < last.Height() {
			chain = append(chain, candidate)
		}
	}
	return chain
}

// BoxTitle names a box by the title written into its top border.
func BoxTitle(grid *Grid, region Region) string {
	runes := []rune(grid.Row(region.Y0))
	if region.X0 >= len(runes) {
		return ""
	}
	end := region.X1 + 1
	if end > len(runes) {
		end = len(runes)
	}
	title := strings.Trim(string(runes[region.X0:end]), "┌┐└┘─━═╭╮╰╯ ")
	title = strings.TrimSpace(title)
	title = strings.Join(strings.Fields(title), " ")
	if len([]rune(title)) > 24 {
		return string([]rune(title)[:24]) + "..."
	}
	return title
}

// borderFiller reports whether a character may appear inside a horizontal border,
// which includes the title an interface writes into it.
func borderFiller(char rune) bool {
	if unicode.IsLetter(char) || unicode.IsDigit(char) {
		return true
	}
	if strings.ContainsRune(" .:,;-_/()[]<>*+|=→·—'\"", char) {
		return true
	}
	if strings.ContainsRune("─━═┄┅┬┴┼", char) {
		return true
	}
	return char > 127 && char != '┐' && char != '┘'
}

func area(region Region) int { return region.Width() * region.Height() }

func charAt(grid *Grid, x, y int) rune {
	cell, ok := grid.At(x, y)
	if !ok {
		return ' '
	}
	return cell.Char
}

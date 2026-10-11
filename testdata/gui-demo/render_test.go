package main

import (
	"image"
	"testing"
)

// TestRenderProducesContent guards the demo's own drawing: if the renderer
// produced a blank canvas, every GUI test would silently measure nothing.
func TestRenderProducesContent(t *testing.T) {
	canvas := render(&state{width: 900, height: 560, tab: 1, selection: 2})
	colours := map[uint32]int{}
	bounds := canvas.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y += 2 {
		for x := bounds.Min.X; x < bounds.Max.X; x += 2 {
			r, g, b, _ := canvas.At(x, y).RGBA()
			colours[uint32(r>>8)<<16|uint32(g>>8)<<8|uint32(b>>8)]++
		}
	}
	if len(colours) < 4 {
		t.Fatalf("expected a drawn interface, got %d distinct colours", len(colours))
	}
	// The blinking dot and the progressive bar must be visible somewhere.
	if !hasColour(canvas, colWarn) {
		t.Error("warning-coloured element (recording dot) missing")
	}
	if !hasColour(canvas, colAccent) {
		t.Error("accent-coloured element (progress bar / selection) missing")
	}
}

func hasColour(img image.Image, want interface{ RGBA() (uint32, uint32, uint32, uint32) }) bool {
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			wr, wg, wb, _ := want.RGBA()
			if r == wr && g == wg && b == wb {
				return true
			}
		}
	}
	return false
}

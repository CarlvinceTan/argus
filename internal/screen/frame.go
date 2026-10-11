package screen

import (
	"image"
	"time"
)

// Frame is one captured GUI screen. It lives in this package because both the
// harness (which produces frames) and the sensors (which measure them) depend on
// it, and neither may depend on the other.
type Frame struct {
	T             time.Duration
	Digest        string
	Image         *image.RGBA
	PNG           []byte
	Width         int
	Height        int
	ChangedPixels int
	TotalPixels   int
}

// InputEvent is one keystroke, click or resize sent to a GUI subject.
type InputEvent struct {
	T    time.Duration `json:"t_ms"`
	Seq  int           `json:"seq"`
	Kind string        `json:"kind"`
	Data string        `json:"data"`
}

// FrameEvent is one observed terminal screen change (the TUI harness ledger).
type FrameEvent struct {
	T            time.Duration `json:"t_ms"`
	Digest       string        `json:"digest"`
	ChangedCells int           `json:"changed_cells"`
	GridID       int           `json:"grid_id"`
	InputSeq     int           `json:"input_seq"`
}

// FrameIndex is a stable id for a frame in a run, derived from its timestamp.
func (f *Frame) FrameIndex() int {
	if f == nil {
		return 0
	}
	return int(f.T / (100 * time.Millisecond))
}

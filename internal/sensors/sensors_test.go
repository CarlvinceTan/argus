package sensors

import (
	"strings"
	"testing"
	"time"

	"github.com/hinshun/vt10x"

	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/screen"
)

func hasCheck(evidence []Evidence, check string) bool {
	for _, item := range evidence {
		if item.Check == check {
			return true
		}
	}
	return false
}

func TestDuplicateLineDetected(t *testing.T) {
	grid := screen.NewGrid([]string{
		"┌──────────────────────────┐",
		"│ Loading configuration... │",
		"│ Loading configuration... │",
		"│ done                     │",
		"└──────────────────────────┘",
	})
	evidence := RunChecks(grid, DefaultCheckOptions())
	if !hasCheck(evidence, CheckDuplicateLine) {
		t.Fatalf("expected duplicate_line, got %+v", evidence)
	}
}

func TestEscapeArtifactDetected(t *testing.T) {
	grid := screen.NewGrid([]string{
		"status: [32mready[0m",
		"path: %!s(string=oops)",
	})
	evidence := RunChecks(grid, DefaultCheckOptions())
	if !hasCheck(evidence, CheckEscapeArtifact) {
		t.Fatalf("expected escape_artifact, got %+v", evidence)
	}
}

func TestHardClipDetected(t *testing.T) {
	row := strings.Repeat("x", 40)
	grid := screen.NewGrid([]string{row, "short", strings.Repeat(" ", 40)})
	evidence := RunChecks(grid, DefaultCheckOptions())
	if !hasCheck(evidence, CheckHardClip) {
		t.Fatalf("expected hard_clip, got %+v", evidence)
	}
}

func TestBorderGapDetected(t *testing.T) {
	grid := screen.NewGrid([]string{
		"┌──────────────────────────────┐",
		"  missing left edge            │",
		"│ also missing right            ",
		"└──────────────────────────────┘",
	})
	evidence := RunChecks(grid, DefaultCheckOptions())
	if !hasCheck(evidence, CheckBorderGap) {
		t.Fatalf("expected border_gap, got %+v", evidence)
	}
}

func TestInvisibleAndLowContrastText(t *testing.T) {
	grid := screen.NewGrid([]string{"invisible text here", "dim text here"})
	grid = grid.WithCell(0, 0, screen.Cell{Char: 'i', FG: vt10x.Color(0), BG: vt10x.Color(0)})
	grid = grid.WithCell(0, 1, screen.Cell{Char: 'd', FG: vt10x.Color(0), BG: vt10x.Color(232)})
	evidence := RunChecks(grid, DefaultCheckOptions())
	if !hasCheck(evidence, CheckInvisibleText) {
		t.Fatalf("expected invisible_text, got %+v", evidence)
	}
}

func TestProgressLabelMismatch(t *testing.T) {
	bar := strings.Repeat("█", 5) + strings.Repeat("░", 15)
	grid := screen.NewGrid([]string{"downloading " + bar + " 90%"})
	evidence := RunChecks(grid, DefaultCheckOptions())
	if !hasCheck(evidence, CheckProgressLabel) {
		t.Fatalf("expected progress_label_mismatch, got %+v", evidence)
	}
}

func TestTableMisalignmentDetected(t *testing.T) {
	grid := screen.NewGrid([]string{
		"name        status    uptime",
		"api-gateway healthy   12d",
		"cache       healthy   4d",
		"worker      degraded       9h",
		"metrics     healthy   2d",
	})
	evidence := RunChecks(grid, DefaultCheckOptions())
	if !hasCheck(evidence, CheckTableMisalign) {
		t.Fatalf("expected table_misalignment, got %+v", evidence)
	}
}

func TestCleanScreenProducesNoEvidence(t *testing.T) {
	grid := screen.NewGrid([]string{
		"┌─ Sessions ──────────────────┐",
		"│ api-gateway   healthy  12d  │",
		"│ cache         healthy   4d  │",
		"│ worker        healthy   9h  │",
		"└─────────────────────────────┘",
	})
	evidence := RunChecks(grid, DefaultCheckOptions())
	for _, item := range evidence {
		if item.Check != CheckRaggedPanel {
			t.Fatalf("clean screen produced evidence %+v", item)
		}
	}
}

func TestFlickerDetectedAcrossFrames(t *testing.T) {
	base := []string{"status: idle          ", "value: 0              "}
	grids := []*screen.Grid{}
	for index := 0; index < 8; index++ {
		grid := screen.NewGrid(base)
		grid.Time = time.Duration(index*80) * time.Millisecond
		grid.ID = index
		if index%2 == 1 {
			grid = grid.WithCell(8, 0, screen.Cell{Char: 'X', FG: vt10x.DefaultFG, BG: vt10x.DefaultBG})
		}
		grids = append(grids, grid)
	}
	report := AnalyzeMotion(grids, nil, nil, config.Default().Thresholds)
	if !hasCheck(report.Evidence, CheckFlicker) {
		t.Fatalf("expected flicker, got %+v", report.Evidence)
	}
}

func TestFullClearDetected(t *testing.T) {
	dense := []string{}
	for index := 0; index < 20; index++ {
		dense = append(dense, "row of content that is long enough to count")
	}
	grids := []*screen.Grid{}
	add := func(rows []string, at time.Duration) {
		grid := screen.NewGrid(rows)
		grid.Time = at
		grid.ID = len(grids)
		grids = append(grids, grid)
	}
	add(dense, 0)
	add([]string{" ", " "}, 60*time.Millisecond)
	add(dense, 120*time.Millisecond)
	add(dense, 180*time.Millisecond)
	report := AnalyzeMotion(grids, nil, nil, config.Default().Thresholds)
	if !hasCheck(report.Evidence, CheckFullClear) {
		t.Fatalf("expected full_screen_clear, got %+v", report.Evidence)
	}
}

func TestIdleChurnDetected(t *testing.T) {
	grids := []*screen.Grid{}
	for index := 0; index < 10; index++ {
		grid := screen.NewGrid([]string{"counting: " + strings.Repeat("x", index), "footer"})
		grid.Time = time.Duration(index*100) * time.Millisecond
		grid.ID = index
		grids = append(grids, grid)
	}
	// No inputs at all: every change is churn.
	report := AnalyzeMotion(grids, nil, nil, config.Default().Thresholds)
	if !hasCheck(report.Evidence, CheckIdleChurn) {
		t.Fatalf("expected idle_redraw_churn, got %+v", report.Evidence)
	}
}

func TestStuckSpinnerDetected(t *testing.T) {
	rows := []string{"⠋ loading data from disk", "press q to quit"}
	grids := []*screen.Grid{}
	for index := 0; index < 6; index++ {
		grid := screen.NewGrid(rows)
		grid.Time = time.Duration(index*200) * time.Millisecond
		grid.ID = index
		grids = append(grids, grid)
	}
	report := AnalyzeMotion(grids, nil, nil, config.Default().Thresholds)
	if !hasCheck(report.Evidence, CheckStuckSpinner) {
		t.Fatalf("expected stuck_spinner, got %+v", report.Evidence)
	}
}

func TestMotionCleanWhenStatic(t *testing.T) {
	rows := []string{"all quiet here", "nothing moves"}
	grids := []*screen.Grid{}
	for index := 0; index < 5; index++ {
		grid := screen.NewGrid(rows)
		grid.Time = time.Duration(index) * time.Second
		grid.ID = index
		grids = append(grids, grid)
	}
	inputs := []screen.InputEvent{{T: 0, Seq: 1, Kind: "key", Data: "\r"}}
	report := AnalyzeMotion(grids, nil, inputs, config.Default().Thresholds)
	if len(report.Evidence) != 0 {
		t.Fatalf("static screen produced motion evidence: %+v", report.Evidence)
	}
}

func TestTruncationWithRoomDetected(t *testing.T) {
	// The exact shape of the fzf 0.65.1 inline-right info bug: a right-aligned
	// value truncated with a marker while 45 columns in front of it are empty.
	row := "> 5" + strings.Repeat(" ", 45) + "probe-info-li·· "
	grid := screen.NewGrid([]string{row, strings.Repeat("-", 63)})
	evidence := RunChecks(grid, DefaultCheckOptions())
	if !hasCheck(evidence, CheckTruncationRoom) {
		t.Fatalf("expected truncation_with_room, got %+v", evidence)
	}
}

func TestTruncationAtEdgeNotReported(t *testing.T) {
	row := "a-very-long-value-that-genuinely-fills-the-entire-terminal-row-width"
	grid := screen.NewGrid([]string{row})
	evidence := RunChecks(grid, DefaultCheckOptions())
	if hasCheck(evidence, CheckTruncationRoom) {
		t.Fatalf("edge truncation should not be reported: %+v", evidence)
	}
}

func TestMarkerRowsAreNotReportedAsHardClips(t *testing.T) {
	// The pre-fix fzf info line carries its own truncation marker; the marker
	// check owns that case, and the clip check must not double-report it.
	row := strings.Repeat(" ", 44) + "probe-info-li··"
	grid := screen.NewGrid([]string{row})
	evidence := RunChecks(grid, DefaultCheckOptions())
	if hasCheck(evidence, CheckHardClip) {
		t.Fatalf("a row with a truncation marker should not be a hard clip: %+v", evidence)
	}
}

func TestRightAlignedValueAfterTextIsNotClipped(t *testing.T) {
	// The fixed fzf 0.65.2 info line: right-aligned, full value, no marker.
	// Correct layout must produce neither a clip nor a truncation finding.
	row := "> 5" + strings.Repeat(" ", 45) + "probe-info-line "
	grid := screen.NewGrid([]string{row})
	evidence := RunChecks(grid, DefaultCheckOptions())
	if hasCheck(evidence, CheckHardClip) || hasCheck(evidence, CheckTruncationRoom) {
		t.Fatalf("a right-aligned full value should produce no truncation evidence: %+v", evidence)
	}
}

func TestRightAlignedValueAtEdgeIsNotClipped(t *testing.T) {
	// fzf 0.65.2 right-aligns its info line: the value ends exactly at the last
	// column, which is the correct layout, not a clip.
	row := strings.Repeat(" ", 44) + "probe-info-line"
	grid := screen.NewGrid([]string{row})
	evidence := RunChecks(grid, DefaultCheckOptions())
	if hasCheck(evidence, CheckHardClip) {
		t.Fatalf("right-aligned value should not be reported as clipped: %+v", evidence)
	}
}

func TestLeftAlignedLongTokenAtEdgeIsClipped(t *testing.T) {
	row := "workspace " + strings.Repeat("x", 44)
	grid := screen.NewGrid([]string{row})
	evidence := RunChecks(grid, DefaultCheckOptions())
	if !hasCheck(evidence, CheckHardClip) {
		t.Fatalf("expected hard_clip for a left-aligned token running off the edge: %+v", evidence)
	}
}

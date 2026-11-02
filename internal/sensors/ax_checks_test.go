package sensors

import (
	"image"
	"image/color"
	"strings"
	"testing"
	"time"

	"github.com/carlvincetan/argus/internal/screen"
)

func axRow(index int, parent *int, role, label string, x, y, w, h float64, actions ...string) screen.AXElement {
	return screen.AXElement{
		Index: index, Parent: parent, Role: role, Label: label,
		Frame: &screen.AXFrame{X: x, Y: y, W: w, H: h}, Actions: actions,
	}
}

func axSnapshot(rows ...screen.AXElement) screen.AXSnapshot {
	root := 0
	elements := []screen.AXElement{axRow(0, nil, "AXWindow", "Polymux", 0, 30, 800, 600)}
	for index, row := range rows {
		row.Index = index + 1
		if row.Parent == nil {
			row.Parent = &root
		}
		elements = append(elements, row)
	}
	return screen.AXSnapshot{Elements: elements, Complete: true, Window: screen.AXFrame{Y: 30, W: 800, H: 600}}
}

func checks(found []Evidence) string {
	names := []string{}
	for _, item := range found {
		names = append(names, item.Check)
	}
	return strings.Join(names, ",")
}

func TestAXChecks(t *testing.T) {
	scroll := 1
	cases := []struct {
		name     string
		snapshot screen.AXSnapshot
		want     string
	}{
		{"clean window", axSnapshot(axRow(0, nil, "AXButton", "Send", 10, 40, 80, 32, "AXPress")), ""},
		{"unnamed control", axSnapshot(axRow(0, nil, "AXButton", "", 10, 40, 80, 32, "AXPress")), AXUnlabelled},
		{"control past the window edge", axSnapshot(axRow(0, nil, "AXButton", "Send", 760, 40, 80, 32, "AXPress")), AXClipped},
		{
			"scrolled row past the edge is normal",
			axSnapshot(
				axRow(0, nil, "AXScrollArea", "", 0, 30, 800, 600),
				axRow(0, &scroll, "AXButton", "Row", 0, 620, 800, 60, "AXPress"),
			),
			"",
		},
		{
			"partly covered control",
			axSnapshot(
				axRow(0, nil, "AXButton", "One", 10, 40, 80, 32, "AXPress"),
				axRow(0, nil, "AXButton", "Two", 60, 40, 80, 32, "AXPress"),
			),
			AXOverlap,
		},
		{
			"control inside a larger one is composition",
			axSnapshot(
				axRow(0, nil, "AXButton", "Header", 10, 40, 300, 70, "AXPress"),
				axRow(0, nil, "AXButton", "Avatar", 40, 50, 40, 40, "AXPress"),
			),
			"",
		},
		{"tiny target", axSnapshot(axRow(0, nil, "AXButton", "Dot", 10, 40, 10, 10, "AXPress")), AXSmallHit},
		{"short label squeezed", axSnapshot(axRow(0, nil, "AXStaticText", "Connected to the server", 10, 40, 40, 16)), AXCutText},
		{"separator dot in a tight frame", axSnapshot(axRow(0, nil, "AXStaticText", "·", 10, 40, 4, 16)), ""},
		{"label that fits", axSnapshot(axRow(0, nil, "AXStaticText", "No messages yet.", 10, 40, 108, 16)), ""},
		{"long prose truncates by design", axSnapshot(axRow(0, nil, "AXStaticText", strings.Repeat("word ", 20), 10, 40, 40, 16)), ""},
		{"blank window", axSnapshot(axRow(0, nil, "AXGroup", "", 0, 30, 800, 600)), AXBlank},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := checks(RunAXChecks(test.snapshot, 1)); got != test.want {
				t.Fatalf("checks = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAXChecksIgnoreIncompleteReads(t *testing.T) {
	snapshot := axSnapshot(axRow(0, nil, "AXButton", "", 10, 40, 80, 32, "AXPress"))
	snapshot.Complete = false
	if found := RunAXChecks(snapshot, 1); len(found) != 0 {
		t.Fatalf("an incomplete read reported %s", checks(found))
	}
}

func TestAXRegionUsesCapturePixels(t *testing.T) {
	snapshot := screen.AXSnapshot{Window: screen.AXFrame{X: 100, Y: 50, W: 400, H: 300}}
	got := snapshot.Region(screen.AXFrame{X: 110, Y: 60, W: 20, H: 10}, 2)
	want := screen.Region{X0: 20, Y0: 20, X1: 59, Y1: 39}
	if got != want {
		t.Fatalf("region = %+v, want %+v", got, want)
	}
}

func reading(at time.Duration, rows ...screen.AXElement) screen.AXReading {
	return screen.AXReading{Snapshot: axSnapshot(rows...), At: at}
}

func TestFlashesReportWhatCameAndWent(t *testing.T) {
	button := axRow(0, nil, "AXButton", "Send", 10, 40, 80, 32, "AXPress")
	loading := axRow(0, nil, "AXStaticText", "Loading chat", 200, 40, 100, 16)
	before := axSnapshot(button)
	flash := []screen.AXReading{reading(0, button), reading(100*time.Millisecond, button, loading), reading(300*time.Millisecond, button)}
	if got := checks(Flashes(before, flash, 1)); got != AXFlash {
		t.Fatalf("a brief loading line gave %q", got)
	}
	real := []screen.AXReading{reading(0, button), reading(100*time.Millisecond, button, loading), reading(1500*time.Millisecond, button)}
	if got := checks(Flashes(before, real, 1)); got != "" {
		t.Fatalf("a real loading state gave %q", got)
	}
	kept := []screen.AXReading{reading(0, button), reading(100*time.Millisecond, button, loading), reading(300*time.Millisecond, button, loading)}
	if got := checks(Flashes(before, kept, 1)); got != "" {
		t.Fatalf("text that stayed gave %q", got)
	}
}

func TestShiftsOnlyCountControlsOutsideThePressedPane(t *testing.T) {
	pane := 1
	build := func(dialY, rowY float64) screen.AXSnapshot {
		snapshot := axSnapshot(
			axRow(0, nil, "AXGroup", "", 0, 30, 200, 600),
			axRow(0, &pane, "AXButton", "Row one", 0, 40, 200, 40, "AXPress"),
			axRow(0, &pane, "AXButton", "Row two", 0, rowY, 200, 40, "AXPress"),
			axRow(0, nil, "AXButton", "Dial", 700, dialY, 40, 40, "AXPress"),
		)
		snapshot.AssignKeys()
		return snapshot
	}
	before := build(40, 90)
	pressed, _ := before.Find("AXButton|Row one#0")
	if got := checks(Shifts(before, build(40, 150), pressed, 1)); got != "" {
		t.Fatalf("a neighbouring row moving gave %q", got)
	}
	if got := checks(Shifts(before, build(80, 90), pressed, 1)); got != AXShift {
		t.Fatalf("a control in another pane moving gave %q", got)
	}
}

func TestShowsContentSeesASplashButNotAFlatWindow(t *testing.T) {
	flat := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			flat.SetRGBA(x, y, color.RGBA{240, 240, 240, 255})
		}
	}
	if ShowsContent(flat) {
		t.Fatal("a flat window counted as content")
	}
	for y := 40; y < 60; y++ {
		for x := 40; x < 60; x++ {
			flat.SetRGBA(x, y, color.RGBA{20, 20, 20, 255})
		}
	}
	if !ShowsContent(flat) {
		t.Fatal("a logo did not count as content")
	}
}

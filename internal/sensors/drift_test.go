package sensors

import (
	"encoding/json"
	"image"
	"image/color"
	"strings"
	"testing"
)

func TestStateRowRoundTripsAsOneLine(t *testing.T) {
	row := StateRow{Role: "AXButton", Name: "a|b", X: 1, Y: 2, W: 3, H: 4, Scrolled: true}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `"AXButton|a|b|1,2,3,4|scrolled"` {
		t.Fatalf("row = %s", raw)
	}
	var back StateRow
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back != row {
		t.Fatalf("back = %+v", back)
	}
}

func TestStateKeepsWhatAPersonWouldNotice(t *testing.T) {
	scroll := 2
	status := axRow(0, nil, "AXStaticText", "connected=1", 0, 0, 10, 10)
	status.Identifier = "app.status"
	snapshot := axSnapshot(
		axRow(0, nil, "AXGroup", "", 0, 30, 800, 600),
		axRow(0, nil, "AXScrollArea", "", 0, 30, 800, 600),
		axRow(0, &scroll, "AXButton", "Row", 0, 60, 800, 40, "AXPress"),
		axRow(0, nil, "AXStaticText", "10.29 pm, 3 unread", 10, 40, 100, 16),
		axRow(0, nil, "AXButton", "", 10, 70, 40, 40, "AXPress"),
		status,
	)
	state := StateOf(snapshot)
	got := []string{}
	for _, row := range state.Rows {
		got = append(got, row.Role+":"+row.Name)
	}
	want := "AXButton:Row AXStaticText:#.# pm, # unread AXButton:"
	if strings.Join(got, " ") != want {
		t.Fatalf("rows = %q", got)
	}
	if !state.Rows[0].Scrolled || state.Rows[1].Scrolled || state.Rows[1].Y != 10 {
		t.Fatalf("rows = %+v", state.Rows)
	}
}

func row(role, name string, x, y, w, h int) StateRow {
	return StateRow{Role: role, Name: name, X: x, Y: y, W: w, H: h}
}

func TestDrift(t *testing.T) {
	send := row("AXButton", "Send", 10, 40, 80, 32)
	name := row("AXTextField", "Name", 10, 90, 120, 28)
	base := State{W: 800, H: 600, Rows: []StateRow{send, name}}
	cases := []struct {
		name string
		got  State
		want string
	}{
		{"same", base, ""},
		{"rounding", State{W: 800, H: 600, Rows: []StateRow{row("AXButton", "Send", 12, 41, 80, 32), name}}, ""},
		{"new row", State{W: 800, H: 600, Rows: []StateRow{send, name, row("AXStaticText", "Model", 10, 120, 40, 16)}}, DriftAdded},
		{"lost control", State{W: 800, H: 600, Rows: []StateRow{name}}, DriftGone},
		{"boxed field", State{W: 800, H: 600, Rows: []StateRow{send, row("AXTextField", "Name", 10, 90, 260, 22)}}, DriftMoved},
		{"renamed", State{W: 800, H: 600, Rows: []StateRow{row("AXButton", "Post", 10, 40, 80, 32), name}}, DriftAdded + "," + DriftGone},
		{"window", State{W: 900, H: 600, Rows: []StateRow{send, name}}, DriftMoved},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := checks(Drift(base, test.got, 1, "startup")); got != test.want {
				t.Fatalf("checks = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDriftOnlyForgivesTheScrollOffset(t *testing.T) {
	old := row("AXTextField", "Name", 0, 100, 148, 21)
	old.Scrolled = true
	moved, resized := old, old
	moved.Y = 700
	resized.W = 260
	want := State{W: 800, H: 600, Rows: []StateRow{old}}
	if found := Drift(want, State{W: 800, H: 600, Rows: []StateRow{moved}}, 1, "scroll"); len(found) != 0 {
		t.Fatalf("scrolling drifted: %+v", found)
	}
	if got := checks(Drift(want, State{W: 800, H: 600, Rows: []StateRow{resized}}, 1, "scroll")); got != DriftMoved {
		t.Fatalf("a resized field in a scroll area gave %q", got)
	}
}

func flat(c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 320, 160))
	for y := 0; y < 160; y++ {
		for x := 0; x < 320; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func TestPixelDrift(t *testing.T) {
	gray := color.RGBA{240, 240, 240, 255}
	dark := color.RGBA{20, 20, 20, 255}
	paint := func(img *image.RGBA, x0, y0, x1, y1 int) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img.SetRGBA(x, y, dark)
			}
		}
	}
	if share, _ := PixelDrift(flat(gray), flat(gray), nil); share != 0 {
		t.Fatalf("identical images differ by %f", share)
	}
	stray := flat(gray)
	paint(stray, 100, 100, 102, 102)
	if share, _ := PixelDrift(flat(gray), stray, nil); share != 0 {
		t.Fatalf("stray pixels counted: %f", share)
	}
	changed := flat(gray)
	paint(changed, 32, 16, 96, 64)
	share, area := PixelDrift(flat(gray), changed, nil)
	if share < 0.05 || area.X0 != 32 || area.Y1 != 63 {
		t.Fatalf("share %f, area %+v", share, area)
	}
	if len(PixelEvidence(share, area, "startup")) != 1 || len(PixelEvidence(pixelShareMax/2, area, "startup")) != 0 {
		t.Fatal("the threshold decides what is reported")
	}
	clock := State{Rows: []StateRow{row("AXStaticText", "#.# pm", 32, 16, 64, 48)}}
	if share, _ := PixelDrift(flat(gray), changed, Masks(clock, 1, nil)); share != 0 {
		t.Fatalf("a clock wasn't masked: %f", share)
	}
	avatar := State{Rows: []StateRow{row("AXImage", "Wedge", 32, 16, 64, 48)}}
	varies := func(name string) bool { return strings.Contains(name, "Wedge") }
	if share, _ := PixelDrift(flat(gray), changed, Masks(avatar, 1, varies)); share != 0 {
		t.Fatalf("a known variation wasn't masked: %f", share)
	}
}

// Command gui-demo is a small X11 GUI program used as a subject for Argus's GUI
// path. It draws everything itself with a bitmap font and talks the X protocol
// directly, so it needs no toolkit and produces identical pixels everywhere.
//
// It deliberately contains the class of defects a real GUI ships:
//
//  1. a label clipped mid-word although the panel is wider (tab 1 header)
//  2. the same row printed twice (tab 1 list)
//  3. low-contrast text (grey on grey, "Last sync")
//  4. two strings drawn over each other ("Building index" / "index ready")
//  5. a blinking recording dot that flickers every frame
//  6. an idle counter that repaints constantly (redraw churn)
//  7. a progress bar filled to 40% while its label says 90%
//  8. an empty panel with no guidance text
//  9. a dropdown that draws without a background, so the rows below show through
//     (revealed by pressing "d")
//  10. a disabled button drawn identically to an enabled one
//
// Keys: 1/2/3 switch tabs, up/down move the selection, d toggles the dropdown,
// q quits. The window is resizable; when narrower than 700px the sidebar text
// overflows its panel.
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"log"
	"os"
	"time"

	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/xproto"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

const (
	defaultWidth  = 900
	defaultHeight = 560
	frameDelay    = 100 * time.Millisecond
)

type state struct {
	width, height int
	tab           int
	selection     int
	dropdown      bool
	blink         bool
	tick          int
	quitting      bool
}

var (
	colBackground = color.RGBA{0x1b, 0x1d, 0x23, 0xff}
	colPanel      = color.RGBA{0x24, 0x27, 0x2f, 0xff}
	colBorder     = color.RGBA{0x3a, 0x3f, 0x4b, 0xff}
	colText       = color.RGBA{0xdd, 0xe2, 0xec, 0xff}
	colDim        = color.RGBA{0x39, 0x3d, 0x46, 0xff} // low contrast on purpose
	colAccent     = color.RGBA{0x6c, 0x9a, 0xff, 0xff}
	colWarn       = color.RGBA{0xff, 0x5c, 0x5c, 0xff}
	colTrack      = color.RGBA{0x2f, 0x33, 0x3c, 0xff}
)

func drawText(dst draw.Image, x, y int, text string, colour color.Color) {
	drawer := &font.Drawer{
		Dst:  dst,
		Src:  &image.Uniform{C: colour},
		Face: basicfont.Face7x13,
		Dot:  fixed.P(x, y),
	}
	drawer.DrawString(text)
}

func drawTextClipped(dst draw.Image, x, y int, text string, colour color.Color, maxPixels int) {
	limit := maxPixels / 7
	if limit < len(text) {
		text = text[:limit]
	}
	drawText(dst, x, y, text, colour)
}

func panel(dst draw.Image, rect image.Rectangle) {
	draw.Draw(dst, rect, &image.Uniform{C: colPanel}, image.Point{}, draw.Src)
	draw.Draw(dst, image.Rect(rect.Min.X, rect.Min.Y, rect.Max.X, rect.Min.Y+1), &image.Uniform{C: colBorder}, image.Point{}, draw.Src)
	draw.Draw(dst, image.Rect(rect.Min.X, rect.Max.Y-1, rect.Max.X, rect.Max.Y), &image.Uniform{C: colBorder}, image.Point{}, draw.Src)
	draw.Draw(dst, image.Rect(rect.Min.X, rect.Min.Y, rect.Min.X+1, rect.Max.Y), &image.Uniform{C: colBorder}, image.Point{}, draw.Src)
	draw.Draw(dst, image.Rect(rect.Max.X-1, rect.Min.Y, rect.Max.X, rect.Max.Y), &image.Uniform{C: colBorder}, image.Point{}, draw.Src)
}

func render(st *state) image.Image {
	canvas := image.NewRGBA(image.Rect(0, 0, st.width, st.height))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: colBackground}, image.Point{}, draw.Src)

	// Header bar.
	header := image.Rect(0, 0, st.width, 34)
	draw.Draw(canvas, header, &image.Uniform{C: colPanel}, image.Point{}, draw.Src)
	drawText(canvas, 12, 22, "TaskBoard", colText)
	// Defect 1: the workspace path is cut inside a panel that is much wider.
	drawTextClipped(canvas, 140, 22, "workspace: /var/lib/taskboard/sessions/2026-05-14", colDim, 210)
	// Defect 5: a blinking recording dot.
	if st.blink {
		draw.Draw(canvas, image.Rect(st.width-40, 12, st.width-26, 26), &image.Uniform{C: colWarn}, image.Point{}, draw.Src)
	}

	// Sidebar and content panels.
	sidebarWidth := 220
	sidebar := image.Rect(12, 46, 12+sidebarWidth, st.height-40)
	content := image.Rect(12+sidebarWidth+12, 46, st.width-12, st.height-40)
	panel(canvas, sidebar)
	panel(canvas, content)

	drawText(canvas, sidebar.Min.X+10, sidebar.Min.Y+22, "Queues", colText)
	queues := []string{"inbox", "builds", "releases", "archive", "trash"}
	for index, name := range queues {
		y := sidebar.Min.Y + 46 + index*20
		colour := colText
		if index == st.selection {
			colour = colAccent
			draw.Draw(canvas, image.Rect(sidebar.Min.X+6, y-14, sidebar.Max.X-6, y+4), &image.Uniform{C: colPanel}, image.Point{}, draw.Src)
		}
		label := "  " + name
		// Defect: when the window is narrow the sidebar text runs past its border.
		if st.width < 700 {
			label = "  " + name + "                "
		}
		drawText(canvas, sidebar.Min.X+10, y, label, colour)
	}
	// Defect 10: a disabled button drawn exactly like an enabled one.
	buttonColor := colAccent
	drawText(canvas, sidebar.Min.X+10, sidebar.Max.Y-14, "[ Archive ]", buttonColor)
	drawText(canvas, sidebar.Min.X+110, sidebar.Max.Y-14, "[ Delete ]", buttonColor)

	switch st.tab {
	case 1:
		drawText(canvas, content.Min.X+10, content.Min.Y+22, "Build log", colText)
		rows := []string{
			"14:02:11  compiling 42 packages",
			"14:02:19  compiling 42 packages", // Defect 2: the same row twice.
			"14:02:31  linking taskboard",
			"14:02:44  writing artifacts",
		}
		for index, row := range rows {
			drawText(canvas, content.Min.X+10, content.Min.Y+48+index*20, row, colText)
		}
		// Defect 4: two strings drawn over one another.
		drawText(canvas, content.Min.X+10, content.Min.Y+140, "Building index", colText)
		drawText(canvas, content.Min.X+24, content.Min.Y+146, "index ready", colWarn)
	case 2:
		drawText(canvas, content.Min.X+10, content.Min.Y+22, "Sync", colText)
		// Defect 3: low-contrast text.
		drawText(canvas, content.Min.X+10, content.Min.Y+48, "Last sync: 4 minutes ago", colDim)
		// Defect 7: progress bar filled 40%, labelled 90%.
		track := image.Rect(content.Min.X+10, content.Min.Y+70, content.Min.X+10+240, content.Min.Y+86)
		draw.Draw(canvas, track, &image.Uniform{C: colTrack}, image.Point{}, draw.Src)
		fill := image.Rect(track.Min.X, track.Min.Y, track.Min.X+96, track.Max.Y)
		draw.Draw(canvas, fill, &image.Uniform{C: colAccent}, image.Point{}, draw.Src)
		drawText(canvas, track.Max.X+12, track.Min.Y+13, "90%", colText)
	default:
		drawText(canvas, content.Min.X+10, content.Min.Y+22, "Overview", colText)
		// Defect 8: an empty panel with no guidance.
		drawText(canvas, content.Min.X+10, content.Min.Y+48, "", colText)
		// Defect 6: a counter that repaints every frame.
		drawText(canvas, content.Min.X+10, content.Min.Y+120, fmt.Sprintf("uptime ticks: %d", st.tick), colText)
	}

	// Dropdown (defect 9: drawn without a background, so rows show through).
	if st.dropdown {
		rect := image.Rect(sidebar.Min.X+6, sidebar.Min.Y+36, sidebar.Min.X+180, sidebar.Min.Y+140)
		draw.Draw(canvas, image.Rect(rect.Min.X, rect.Min.Y, rect.Max.X, rect.Min.Y+1), &image.Uniform{C: colAccent}, image.Point{}, draw.Src)
		for index := 0; index < 4; index++ {
			drawText(canvas, rect.Min.X+4, rect.Min.Y+18+index*20, []string{"new queue", "rename", "merge", "settings"}[index], colText)
		}
	}

	// Footer with tabs.
	footer := image.Rect(0, st.height-34, st.width, st.height)
	draw.Draw(canvas, footer, &image.Uniform{C: colPanel}, image.Point{}, draw.Src)
	tabs := []string{"[1] Overview", "[2] Sync", "[3] Build log"}
	for index, label := range tabs {
		colour := colText
		if index == st.tab {
			colour = colAccent
		}
		drawText(canvas, 12+index*140, st.height-12, label, colour)
	}
	drawText(canvas, st.width-150, st.height-12, "d: menu  q: quit", colDim)
	return canvas
}

func main() {
	display := os.Getenv("DISPLAY")
	if display == "" {
		display = ":99"
	}
	conn, err := xgb.NewConnDisplay(display)
	if err != nil {
		log.Fatalf("gui-demo: cannot connect to X server %s: %v", display, err)
	}
	defer conn.Close()

	setup := xproto.Setup(conn)
	screen := setup.DefaultScreen(conn)
	window, err := xproto.NewWindowId(conn)
	if err != nil {
		log.Fatalf("gui-demo: window id: %v", err)
	}
	gc, err := xproto.NewGcontextId(conn)
	if err != nil {
		log.Fatalf("gui-demo: gc id: %v", err)
	}
	width, height := defaultWidth, defaultHeight
	if err := xproto.CreateWindowChecked(
		conn, screen.RootDepth, window, screen.Root,
		0, 0, uint16(width), uint16(height), 0,
		xproto.WindowClassInputOutput, screen.RootVisual,
		xproto.CwBackPixel|xproto.CwEventMask,
		[]uint32{uint32(colBackground.R)<<16 | uint32(colBackground.G)<<8 | uint32(colBackground.B), xproto.EventMaskExposure | xproto.EventMaskKeyPress | xproto.EventMaskStructureNotify},
	).Check(); err != nil {
		log.Fatalf("gui-demo: create window: %v", err)
	}
	if err := xproto.CreateGCChecked(conn, gc, xproto.Drawable(window), 0, nil).Check(); err != nil {
		log.Fatalf("gui-demo: create gc: %v", err)
	}
	if err := xproto.MapWindowChecked(conn, window).Check(); err != nil {
		log.Fatalf("gui-demo: map window: %v", err)
	}
	// Name the window so a harness can find it among the root's children.
	if err := xproto.ChangePropertyChecked(
		conn, xproto.PropModeReplace, window, xproto.AtomWmName, xproto.AtomString, 8,
		uint32(len("TaskBoard")), []byte("TaskBoard"),
	).Check(); err != nil {
		log.Printf("gui-demo: naming window failed: %v", err)
	}

	st := &state{width: width, height: height, tab: 0}
	frames := make(chan struct{}, 1)

	go func() {
		for {
			event, err := conn.WaitForEvent()
			if err != nil {
				return
			}
			switch typed := event.(type) {
			case xproto.ExposeEvent:
				select {
				case frames <- struct{}{}:
				default:
				}
			case xproto.ConfigureNotifyEvent:
				if typed.Width > 0 && typed.Height > 0 {
					st.width, st.height = int(typed.Width), int(typed.Height)
				}
				select {
				case frames <- struct{}{}:
				default:
				}
			case xproto.KeyPressEvent:
				// Only the low byte matters for this demo's keys.
				switch typed.Detail {
				case 10: // "1"
					st.tab = 0
				case 11: // "2"
					st.tab = 1
				case 12: // "3"
					st.tab = 2
				case 40: // "d"
					st.dropdown = !st.dropdown
				case 38: // "a"
					st.selection = (st.selection + 1) % 5
				case 116, 24: // PageDown, "q"
					if typed.Detail == 24 {
						st.quitting = true
					}
					st.selection = (st.selection + 1) % 5
				case 111: // Up arrow
					if st.selection > 0 {
						st.selection--
					}
				case 116 + 1: // Down arrow
					st.selection = (st.selection + 1) % 5
				case 113: // Left arrow
				case 114: // Right arrow
				}
				select {
				case frames <- struct{}{}:
				default:
				}
			}
		}
	}()

	selfCheck := os.Getenv("GUI_DEMO_SELFCHECK") != ""
	ticker := time.NewTicker(frameDelay)
	defer ticker.Stop()
	fps := 0
	lastReport := time.Now()
	framesDrawn := 0
	for !st.quitting {
		select {
		case <-ticker.C:
		case <-frames:
		}
		st.tick++
		st.blink = !st.blink
		canvas := render(st)
		putImage(conn, window, gc, canvas, screen.RootDepth)
		framesDrawn++
		if selfCheck && framesDrawn%25 == 0 {
			reportSelfCheck(conn, window, st)
		}
		fps++
		if time.Since(lastReport) >= 5*time.Second {
			fmt.Fprintf(os.Stderr, "gui-demo: %d frames, %dx%d\n", fps, st.width, st.height)
			lastReport = time.Now()
		}
	}
}

// reportSelfCheck reads the window back and reports what the server actually
// holds, which distinguishes "the app is not drawing" from "the harness is
// looking at the wrong window".
func reportSelfCheck(conn *xgb.Conn, window xproto.Window, st *state) {
	attributes, err := xproto.GetWindowAttributes(conn, window).Reply()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gui-demo selfcheck: attributes: %v\n", err)
		return
	}
	reply, err := xproto.GetImage(conn, xproto.ImageFormatZPixmap, xproto.Drawable(window),
		0, 0, uint16(st.width), uint16(st.height), ^uint32(0)).Reply()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gui-demo selfcheck: getimage: %v\n", err)
		return
	}
	colours := map[uint32]int{}
	for index := 0; index+3 < len(reply.Data); index += 4 * 97 { // sparse sample
		key := uint32(reply.Data[index+2])<<16 | uint32(reply.Data[index+1])<<8 | uint32(reply.Data[index])
		colours[key]++
	}
	dominant, dominantCount := uint32(0), 0
	for key, count := range colours {
		if count > dominantCount {
			dominant, dominantCount = key, count
		}
	}
	fmt.Fprintf(os.Stderr, "gui-demo selfcheck: map_state=%d depth=%d data=%d bytes, distinct=%d, dominant=#%06x\n",
		attributes.MapState, reply.Depth, len(reply.Data), len(colours), dominant)
}

// putImage converts an image to the X server's ZPixmap layout and uploads it in
// horizontal strips: one request for a window this size would be about 2 MB, well
// past the 256 KB X request limit, and the server rejects it with BadLength.
func putImage(conn *xgb.Conn, window xproto.Window, gc xproto.Gcontext, img image.Image, depth byte) {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	const stripRows = 48 // 48 * 900 * 4 = 172 KB, comfortably inside the limit
	strip := make([]byte, width*stripRows*4)
	for start := 0; start < height; start += stripRows {
		rows := stripRows
		if start+rows > height {
			rows = height - start
		}
		for y := 0; y < rows; y++ {
			for x := 0; x < width; x++ {
				r, g, b, _ := img.At(bounds.Min.X+x, bounds.Min.Y+start+y).RGBA()
				offset := (y*width + x) * 4
				strip[offset+0] = byte(b >> 8)
				strip[offset+1] = byte(g >> 8)
				strip[offset+2] = byte(r >> 8)
				strip[offset+3] = 0
			}
		}
		if err := xproto.PutImageChecked(conn, xproto.ImageFormatZPixmap, xproto.Drawable(window), gc,
			uint16(width), uint16(rows), 0, int16(start), 0, depth, strip[:rows*width*4]).Check(); err != nil {
			fmt.Fprintf(os.Stderr, "gui-demo: putImage strip at y=%d failed: %v\n", start, err)
			return
		}
	}
}

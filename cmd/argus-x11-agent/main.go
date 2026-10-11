// Command argus-x11-agent runs next to the system under test and speaks a small
// JSON-lines protocol over a loopback socket.
//
// The reason this exists: an X11 window is megabytes of pixels per frame, and a
// test harness that pulls those pixels across the network is unusable (measured
// at about 1.4 MB/s over a tunnel, which is several seconds per frame). Instead
// the agent captures locally, keeps the frame ledger, and sends only what the
// analyser needs: change metrics for every frame, plus the PNG of a frame when
// it actually changed.
//
// The agent listens on loopback, so an evaluation sandbox that permits only
// loopback still reaches it through a forwarded port.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"time"

	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/harness"
	"github.com/carlvincetan/argus/internal/sensors"
)

type request struct {
	Cmd        string `json:"cmd"`
	Key        string `json:"key"`
	X          int    `json:"x"`
	Y          int    `json:"y"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	IncludePNG bool   `json:"include_png"`
	Window     string `json:"window"`
	DurationMS int    `json:"duration_ms"`
	IntervalMS int    `json:"interval_ms"`
}

type evidencePayload struct {
	Check      string  `json:"check"`
	X0         int     `json:"x0"`
	Y0         int     `json:"y0"`
	X1         int     `json:"x1"`
	Y1         int     `json:"y1"`
	Detail     string  `json:"detail"`
	Severity   float64 `json:"severity_hint"`
	Confidence float64 `json:"confidence"`
	Measured   float64 `json:"measured"`
	GridID     int     `json:"grid_id"`
}

type framePayload struct {
	T             int64  `json:"t_ms"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	Digest        string `json:"digest"`
	ChangedPixels int    `json:"changed_pixels"`
	TotalPixels   int    `json:"total_pixels"`
	PNG           string `json:"png,omitempty"`
}

type response struct {
	OK          bool                 `json:"ok"`
	Error       string               `json:"error,omitempty"`
	Frame       *framePayload        `json:"frame,omitempty"`
	Windows     []harness.WindowInfo `json:"windows,omitempty"`
	Evidence    []evidencePayload    `json:"evidence,omitempty"`
	Frames      []framePayload       `json:"frames,omitempty"`
	ChangedRate float64              `json:"changed_rate,omitempty"`
	Width       int                  `json:"width,omitempty"`
	Height      int                  `json:"height,omitempty"`
	Note        string               `json:"note,omitempty"`
}

func main() {
	port := flag.Int("port", 7011, "loopback port to listen on")
	display := flag.String("display", ":99", "X display to observe")
	windowMatch := flag.String("window", "", "WM_NAME substring of the subject window")
	flag.Parse()

	server, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Fatalf("argus-x11-agent: listening: %v", err)
	}
	log.Printf("argus-x11-agent: listening on 127.0.0.1:%d, display %s", *port, *display)

	for {
		connection, err := server.Accept()
		if err != nil {
			log.Printf("argus-x11-agent: accept: %v", err)
			continue
		}
		go serve(connection, *display, *windowMatch)
	}
}

func serve(connection net.Conn, display, windowMatch string) {
	defer connection.Close()
	encoder := json.NewEncoder(connection)
	decoder := json.NewDecoder(bufio.NewReader(connection))

	var x11 *harness.X11Harness
	ensure := func(window string) error {
		if x11 != nil {
			return nil
		}
		match := windowMatch
		if window != "" {
			match = window
		}
		started, err := harness.StartX11(harness.X11Options{Display: display, WindowMatch: match})
		if err != nil {
			return err
		}
		x11 = started
		return nil
	}
	defer func() {
		if x11 != nil {
			_ = x11.Close()
		}
	}()

	for {
		var req request
		if err := decoder.Decode(&req); err != nil {
			return
		}
		reply := response{OK: true}
		switch req.Cmd {
		case "capture":
			if err := ensure(req.Window); err != nil {
				reply.OK, reply.Error = false, err.Error()
				break
			}
			frame, err := x11.Capture()
			if err != nil {
				reply.OK, reply.Error = false, err.Error()
				break
			}
			reply.Frame = &framePayload{
				T:             frame.T.Milliseconds(),
				Width:         frame.Width,
				Height:        frame.Height,
				Digest:        frame.Digest,
				ChangedPixels: frame.ChangedPixels,
				TotalPixels:   frame.TotalPixels,
			}
			if req.IncludePNG {
				reply.Frame.PNG = base64.StdEncoding.EncodeToString(frame.PNG)
			}
		case "key":
			if err := ensure(""); err != nil {
				reply.OK, reply.Error = false, err.Error()
				break
			}
			if err := x11.SendKey(req.Key); err != nil {
				reply.OK, reply.Error = false, err.Error()
			}
		case "click":
			if err := ensure(""); err != nil {
				reply.OK, reply.Error = false, err.Error()
				break
			}
			if err := x11.Click(req.X, req.Y); err != nil {
				reply.OK, reply.Error = false, err.Error()
			}
		case "resize":
			if err := ensure(""); err != nil {
				reply.OK, reply.Error = false, err.Error()
				break
			}
			if err := x11.ResizeWindow(req.Width, req.Height); err != nil {
				reply.OK, reply.Error = false, err.Error()
			}
		case "size":
			if err := ensure(""); err != nil {
				reply.OK, reply.Error = false, err.Error()
				break
			}
			width, height := x11.Size()
			reply.Width, reply.Height = width, height
		case "refind":
			if x11 != nil {
				if err := x11.Refind(); err != nil {
					reply.OK, reply.Error = false, err.Error()
					break
				}
			} else if err := ensure(req.Window); err != nil {
				reply.OK, reply.Error = false, err.Error()
			}
		case "analyze":
			if err := ensure(req.Window); err != nil {
				reply.OK, reply.Error = false, err.Error()
				break
			}
			duration := req.DurationMS
			if duration <= 0 {
				duration = 1500
			}
			interval := req.IntervalMS
			if interval <= 0 {
				interval = 120
			}
			// Sampling happens here, next to the X server: over a network link
			// a 10 Hz animation cannot be sampled without aliasing, so the
			// sensor runs where the pixels are and only its conclusions travel.
			window := []*harness.X11Frame{}
			deadline := time.Now().Add(time.Duration(duration) * time.Millisecond)
			for time.Now().Before(deadline) {
				frame, err := x11.Capture()
				if err != nil {
					break
				}
				window = append(window, frame)
				time.Sleep(time.Duration(interval) * time.Millisecond)
			}
			if len(window) == 0 {
				reply.OK, reply.Error = false, "no frames captured"
				break
			}
			report := sensors.AnalyzePixels(window, x11.Inputs(), config.Default().Thresholds)
			reply.ChangedRate = report.ChangedPixelsPerSec
			for _, item := range report.Evidence {
				reply.Evidence = append(reply.Evidence, evidencePayload{
					Check: item.Check, X0: item.Region.X0, Y0: item.Region.Y0, X1: item.Region.X1, Y1: item.Region.Y1,
					Detail: item.Detail, Severity: item.Severity, Confidence: item.Confidence, Measured: item.Measured, GridID: item.GridID,
				})
			}
			// Send the frames with the biggest changes as visual evidence.
			sorted := append([]*harness.X11Frame{}, window...)
			sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ChangedPixels > sorted[j].ChangedPixels })
			for index, frame := range sorted {
				if index >= 4 {
					break
				}
				reply.Frames = append(reply.Frames, framePayload{
					T: frame.T.Milliseconds(), Width: frame.Width, Height: frame.Height,
					Digest: frame.Digest, ChangedPixels: frame.ChangedPixels, TotalPixels: frame.TotalPixels,
					PNG: base64.StdEncoding.EncodeToString(frame.PNG),
				})
			}
		case "windows":
			windows, err := harness.ListWindows(display)
			if err != nil {
				reply.OK, reply.Error = false, err.Error()
				break
			}
			reply.Windows = windows
		case "ping":
			reply.Note = "pong"
		case "close":
			reply.Note = "closing"
			_ = encoder.Encode(reply)
			return
		default:
			reply.OK, reply.Error = false, "unknown command "+req.Cmd
		}
		if err := encoder.Encode(reply); err != nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

var _ = os.Stderr

// Package report writes the artefacts a reviewer (or the visualiser) reads:
// run.json, a markdown backlog, annotated stills, temporal strips and video
// clips. Artefacts are the only thing that survives a run, so they are written
// even when the session is stopped early.
package report

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/carlvincetan/argus/internal/harness"
	"github.com/carlvincetan/argus/internal/run"
	"github.com/carlvincetan/argus/internal/screen"
)

// Writer lays out the artefact directory for one session.
type Writer struct {
	Dir       string
	StillsDir string
	StripsDir string
	ClipsDir  string
	Opts      screen.RenderOptions
	Video     bool
	Grids     map[int]*screen.Grid
	gridList  []*screen.Grid
	// GUI runs carry pixel frames instead of terminal grids; the writer keeps
	// both so evidence rendering can branch per session kind.
	Images     map[int]*harness.X11Frame
	imageList  []*harness.X11Frame
	kind       string
	FFmpegPath string
}

// NewWriter prepares the artefact directories.
func NewWriter(dir string, video bool) (*Writer, error) {
	w := &Writer{
		Dir:       dir,
		StillsDir: filepath.Join(dir, "stills"),
		StripsDir: filepath.Join(dir, "strips"),
		ClipsDir:  filepath.Join(dir, "clips"),
		Opts:      screen.DefaultRenderOptions(),
		Video:     video,
		Grids:     map[int]*screen.Grid{},
		Images:    map[int]*harness.X11Frame{},
	}
	for _, sub := range []string{w.Dir, w.StillsDir, w.StripsDir, w.ClipsDir} {
		if err := os.MkdirAll(sub, 0o755); err != nil {
			return nil, fmt.Errorf("creating %s: %w", sub, err)
		}
	}
	if path, err := findFFmpeg(); err == nil {
		w.FFmpegPath = path
	}
	return w, nil
}

// Index registers the grids captured during a session.
func (w *Writer) Index(grids []*screen.Grid) {
	w.Grids = make(map[int]*screen.Grid, len(grids))
	w.gridList = append([]*screen.Grid{}, grids...)
	for _, grid := range grids {
		w.Grids[grid.ID] = grid
	}
}

// IndexImages registers the pixel frames captured during a GUI session.
func (w *Writer) IndexImages(frames []*harness.X11Frame) {
	w.Images = make(map[int]*harness.X11Frame, len(frames))
	w.imageList = append([]*harness.X11Frame{}, frames...)
	for index, frame := range frames {
		w.Images[index] = frame
	}
}

// WriteSession writes run.json, report.md and per-finding evidence.
func (w *Writer) WriteSession(session *run.Session) error {
	w.kind = session.Kind
	if session.Kind == run.KindGUI {
		if err := w.writeGUISession(session); err != nil {
			return err
		}
	}
	return w.writeCommon(session)
}

// writeGUISession renders evidence for GUI findings: annotated pixels, frame
// strips and clips.
func (w *Writer) writeGUISession(session *run.Session) error {
	for index := range session.Findings {
		finding := &session.Findings[index]
		if err := w.writeGUIEvidence(finding); err != nil {
			session.AddLog("evidence for %s failed: %v", finding.ID, err)
		}
	}
	if w.Video && len(w.imageList) > 1 {
		clip := filepath.Join(w.ClipsDir, "session.mp4")
		if err := w.writeImageClip(sampleEveryImage(w.imageList, max(1, len(w.imageList)/120)), clip, screen.Region{}, color.RGBA{}, clipFPS); err != nil {
			session.AddLog("session clip unavailable: %v", err)
		}
	}
	return nil
}

func (w *Writer) writeGUIEvidence(finding *run.Finding) error {
	frames := []*harness.X11Frame{}
	for _, frame := range w.imageList {
		if frame.T >= finding.FirstSeen-2*time.Second && frame.T <= finding.LastSeen+2*time.Second {
			frames = append(frames, frame)
		}
	}
	if len(frames) == 0 && len(w.imageList) > 0 {
		frames = append(frames, w.imageList[len(w.imageList)-1])
	}
	if len(frames) == 0 {
		return nil
	}
	accent := severityColour(finding.Severity)
	still := frames[len(frames)-1]
	// Steps run closer together than this window, so prefer the frame it was seen on.
	for _, frame := range frames {
		if frame.T == finding.LastSeen {
			still = frame
		}
	}
	stillBytes := screen.EncodeImagePNG(screen.AnnotateImage(still.Image, finding.Region, accent))
	stillName := fmt.Sprintf("stills/%s.png", finding.ID)
	if err := os.WriteFile(filepath.Join(w.Dir, stillName), stillBytes, 0o644); err != nil {
		return err
	}
	finding.Still = stillName

	cropRegion := padPixelRegion(finding.Region, still.Width, still.Height, 12)
	cropped := cropImage(still.Image, cropRegion)
	highlight := screen.Region{
		X0: finding.Region.X0 - cropRegion.X0,
		Y0: finding.Region.Y0 - cropRegion.Y0,
		X1: finding.Region.X1 - cropRegion.X0,
		Y1: finding.Region.Y1 - cropRegion.Y0,
	}
	cropName := fmt.Sprintf("stills/%s-crop.png", finding.ID)
	if err := os.WriteFile(filepath.Join(w.Dir, cropName), screen.EncodeImagePNG(screen.AnnotateImage(cropped, highlight, accent)), 0o644); err != nil {
		return err
	}
	finding.Crop = cropName

	sampled := sampleFramesImage(frames, 4)
	if len(sampled) >= 2 {
		images := make([]image.Image, 0, len(sampled))
		for _, frame := range sampled {
			images = append(images, screen.AnnotateImage(frame.Image, finding.Region, accent))
		}
		stripName := fmt.Sprintf("strips/%s.png", finding.ID)
		if err := os.WriteFile(filepath.Join(w.Dir, stripName), screen.CombineImages(images, color.RGBA{}), 0o644); err != nil {
			return err
		}
		finding.Strip = stripName
		if w.Video && w.FFmpegPath != "" {
			clipName := filepath.Join(w.ClipsDir, finding.ID+".mp4")
			if err := w.writeImageClip(sampled, clipName, finding.Region, accent, clipFPS); err == nil {
				finding.Clip = filepath.Join("clips", finding.ID+".mp4")
			}
		}
	}
	return nil
}

func (w *Writer) writeCommon(session *run.Session) error {
	for index := range session.Findings {
		finding := &session.Findings[index]
		if err := w.writeEvidence(finding); err != nil {
			session.AddLog("evidence for %s failed: %v", finding.ID, err)
		}
	}
	if w.Video && session.Duration < 3*time.Minute {
		clip := filepath.Join(w.ClipsDir, "session.mp4")
		if err := w.WriteTimelineClip(clip, 8, 600); err != nil {
			session.AddLog("session clip unavailable: %v", err)
		}
	}
	if err := w.writeJSON(filepath.Join(w.Dir, "run.json"), session); err != nil {
		return err
	}
	markdown := RenderMarkdown(session)
	if err := os.WriteFile(filepath.Join(w.Dir, "report.md"), []byte(markdown), 0o644); err != nil {
		return fmt.Errorf("writing report.md: %w", err)
	}
	return nil
}

func (w *Writer) writeEvidence(finding *run.Finding) error {
	grid := w.gridFor(finding)
	if grid == nil {
		return nil
	}
	annotation := screen.Annotation{
		Region: finding.Region,
		Colour: severityColour(finding.Severity),
		Filled: true,
	}
	still, err := screen.RenderAnnotated(grid, []screen.Annotation{annotation}, w.Opts)
	if err != nil {
		return err
	}
	stillName := fmt.Sprintf("stills/%s.png", finding.ID)
	if err := os.WriteFile(filepath.Join(w.Dir, stillName), still, 0o644); err != nil {
		return err
	}
	finding.Still = stillName

	cropRegion := padRegion(finding.Region, grid, 10, 2)
	crop := grid.Crop(cropRegion.X0, cropRegion.Y0, cropRegion.X1+1, cropRegion.Y1+1)
	highlighted := screen.Region{
		X0: finding.Region.X0 - cropRegion.X0,
		Y0: finding.Region.Y0 - cropRegion.Y0,
		X1: finding.Region.X1 - cropRegion.X0,
		Y1: finding.Region.Y1 - cropRegion.Y0,
	}
	cropped, err := screen.RenderAnnotated(crop, []screen.Annotation{{
		Region: highlighted,
		Colour: severityColour(finding.Severity),
		Filled: true,
	}}, w.Opts)
	if err != nil {
		return err
	}
	cropName := fmt.Sprintf("stills/%s-crop.png", finding.ID)
	if err := os.WriteFile(filepath.Join(w.Dir, cropName), cropped, 0o644); err != nil {
		return err
	}
	finding.Crop = cropName

	frames := w.windowFrames(finding, 2*time.Second)
	if len(frames) >= 2 {
		strip := frames
		if len(strip) > 6 {
			strip = sampleFrames(strip, 6)
		}
		stripBytes, err := screen.RenderAnnotatedStrip(strip, []screen.Annotation{{
			Region: finding.Region,
			Colour: severityColour(finding.Severity),
			Filled: true,
		}}, w.Opts)
		if err == nil {
			stripName := fmt.Sprintf("strips/%s.png", finding.ID)
			if err := os.WriteFile(filepath.Join(w.Dir, stripName), stripBytes, 0o644); err != nil {
				return err
			}
			finding.Strip = stripName
		}
		finding.Frames = gridIDs(frames)
		if w.Video && w.FFmpegPath != "" && (finding.Class == "animation" || finding.Class == "layout_jitter" || finding.Severity >= run.SevMajor) {
			clipName := filepath.Join(w.ClipsDir, finding.ID+".mp4")
			if err := w.writeFramesClip(frames, clipName, w.Opts, finding.Region, severityColour(finding.Severity), 8); err == nil {
				finding.Clip = filepath.Join("clips", finding.ID+".mp4")
			}
		}
	}
	return nil
}

// gridFor resolves the frame a finding points at, falling back to the closest
// retained frame by timestamp (grids are evicted from the ring buffer on long
// runs, and a finding must still have evidence).
func (w *Writer) gridFor(finding *run.Finding) *screen.Grid {
	if grid := w.Grids[finding.GridID]; grid != nil {
		return grid
	}
	var best *screen.Grid
	bestDelta := time.Duration(1<<62 - 1)
	for _, grid := range w.gridList {
		delta := grid.Time - finding.LastSeen
		if delta < 0 {
			delta = -delta
		}
		if delta < bestDelta {
			best, bestDelta = grid, delta
		}
	}
	return best
}

// windowFrames returns the grids observed inside the finding's time window.
func (w *Writer) windowFrames(finding *run.Finding, pad time.Duration) []*screen.Grid {
	out := []*screen.Grid{}
	for _, grid := range w.gridList {
		if grid.Time >= finding.FirstSeen-pad && grid.Time <= finding.LastSeen+pad {
			out = append(out, grid)
		}
	}
	if len(out) == 0 {
		if grid := w.Grids[finding.GridID]; grid != nil {
			out = append(out, grid)
		}
	}
	return out
}

func (w *Writer) writeJSON(path string, payload any) error {
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding session: %w", err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// RenderMarkdown produces the human-readable backlog: flat by severity, then
// grouped by page or feature.
func RenderMarkdown(session *run.Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Argus report: %s\n\n", session.Subject.Name)
	fmt.Fprintf(&b, "- run `%s`, mode `%s`, terminal %dx%d\n", session.ID, session.Mode, session.Terminal[0], session.Terminal[1])
	fmt.Fprintf(&b, "- duration %s, %d observations, %d frames, %d actions\n",
		session.Duration.Round(time.Millisecond), session.Stats.Observations, session.Stats.Frames, session.Stats.Actions)
	fmt.Fprintf(&b, "- decision calls %d, vision calls %d, cost $%.5f\n",
		session.Cost.DecisionCalls, session.Cost.VisionCalls, session.Cost.CostUSD)
	if session.Subject.Version != "" || session.Subject.Commit != "" {
		fmt.Fprintf(&b, "- subject version %q commit %q\n", session.Subject.Version, session.Subject.Commit)
	}
	if len(session.Notes) > 0 {
		b.WriteString("\nNotes:\n")
		for _, note := range session.Notes {
			fmt.Fprintf(&b, "- %s\n", note)
		}
	}

	reported := []run.Finding{}
	for _, finding := range session.Findings {
		if finding.Reported {
			reported = append(reported, finding)
		}
	}
	fmt.Fprintf(&b, "\n## Backlog (%d reported of %d candidates-by-severity)\n\n", len(reported), len(session.Findings))
	if len(reported) == 0 {
		b.WriteString("No issues met the reporting threshold in this run.\n")
	}
	for _, finding := range reported {
		fmt.Fprintf(&b, "### [%s] %s\n\n", strings.ToUpper(finding.SeverityName), finding.Title)
		fmt.Fprintf(&b, "%s\n\n", finding.Brief)
		fmt.Fprintf(&b, "- group: `%s` / screen: `%s`\n", finding.Group, finding.Screen)
		fmt.Fprintf(&b, "- region: %s, seen %d time(s) from %s to %s\n",
			finding.Region, finding.Events, finding.FirstSeen.Round(time.Millisecond), finding.LastSeen.Round(time.Millisecond))
		fmt.Fprintf(&b, "- defect probability %.2f, confidence %.2f, verified `%s`%s\n",
			finding.DefectProb, finding.Confidence, finding.Verified, intentionalNote(finding))
		if finding.Detail != "" {
			fmt.Fprintf(&b, "- detail: %s\n", finding.Detail)
		}
		if finding.Still != "" {
			fmt.Fprintf(&b, "- evidence: `%s`", finding.Still)
			if finding.Crop != "" {
				fmt.Fprintf(&b, ", `%s`", finding.Crop)
			}
			if finding.Strip != "" {
				fmt.Fprintf(&b, ", `%s`", finding.Strip)
			}
			if finding.Clip != "" {
				fmt.Fprintf(&b, ", `%s`", finding.Clip)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	groups := run.GroupFindings(reported)
	if len(groups) > 0 {
		b.WriteString("## By page/feature\n\n")
		names := make([]string, 0, len(groups))
		for name := range groups {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			findings := groups[name]
			run.SortFindings(findings)
			worst := findings[0].SeverityName
			fmt.Fprintf(&b, "### %s (%d findings, worst: %s)\n\n", name, len(findings), worst)
			for _, finding := range findings {
				fmt.Fprintf(&b, "- [%s] %s — %s\n", finding.SeverityName, finding.Title, finding.Screen)
			}
			b.WriteString("\n")
		}
	}

	rejected := []run.Finding{}
	for _, finding := range session.Findings {
		if !finding.Reported {
			rejected = append(rejected, finding)
		}
	}
	if len(rejected) > 0 {
		fmt.Fprintf(&b, "## Below the reporting threshold (%d)\n\n", len(rejected))
		for _, finding := range rejected {
			fmt.Fprintf(&b, "- [%s, p=%.2f] %s (%s)\n", finding.SeverityName, finding.DefectProb, finding.Title, finding.Group)
		}
		b.WriteString("\n")
	}
	if len(session.Log) > 0 {
		b.WriteString("## Run log\n\n```\n")
		for _, line := range session.Log {
			b.WriteString(line)
			b.WriteString("\n")
		}
		b.WriteString("```\n")
	}
	return b.String()
}

func intentionalNote(finding run.Finding) string {
	if finding.Intentional {
		return ", marked intentional"
	}
	return ""
}

func severityColour(severity run.Severity) color.RGBA {
	switch severity {
	case run.SevCritical:
		return screen.AnnotationPalette["critical"]
	case run.SevMajor:
		return screen.AnnotationPalette["major"]
	case run.SevNoticeable:
		return screen.AnnotationPalette["minor"]
	default:
		return screen.AnnotationPalette["info"]
	}
}

// padPixelRegion grows a pixel region, clamped to the image.
func padPixelRegion(region screen.Region, width, height, pad int) screen.Region {
	out := screen.Region{X0: region.X0 - pad, Y0: region.Y0 - pad, X1: region.X1 + pad, Y1: region.Y1 + pad}
	if out.X0 < 0 {
		out.X0 = 0
	}
	if out.Y0 < 0 {
		out.Y0 = 0
	}
	if out.X1 >= width {
		out.X1 = width - 1
	}
	if out.Y1 >= height {
		out.Y1 = height - 1
	}
	if out.X1 < out.X0 {
		out.X1 = out.X0
	}
	if out.Y1 < out.Y0 {
		out.Y1 = out.Y0
	}
	return out
}

// cropImage copies a pixel region out of an image.
func cropImage(img *image.RGBA, region screen.Region) *image.RGBA {
	if img == nil {
		return nil
	}
	rect := image.Rect(region.X0, region.Y0, region.X1+1, region.Y1+1).Intersect(img.Bounds())
	if rect.Empty() {
		return img
	}
	out := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(out, out.Bounds(), img, rect.Min, draw.Src)
	return out
}

// sampleFramesImage samples a pixel frame list down to a manageable count.
func sampleFramesImage(frames []*harness.X11Frame, count int) []*harness.X11Frame {
	if len(frames) <= count {
		return frames
	}
	out := make([]*harness.X11Frame, 0, count)
	for index := 0; index < count; index++ {
		out = append(out, frames[index*(len(frames)-1)/(count-1)])
	}
	return out
}

// sampleEveryImage thins a frame list for the session clip.
func sampleEveryImage(frames []*harness.X11Frame, step int) []*harness.X11Frame {
	if step < 1 || len(frames) <= maxClipFrames {
		return frames
	}
	out := []*harness.X11Frame{}
	for index := 0; index < len(frames); index += step {
		out = append(out, frames[index])
	}
	return out
}

func padRegion(region screen.Region, grid *screen.Grid, padX, padY int) screen.Region {
	out := screen.Region{
		X0: region.X0 - padX,
		Y0: region.Y0 - padY,
		X1: region.X1 + padX,
		Y1: region.Y1 + padY,
	}
	if out.X0 < 0 {
		out.X0 = 0
	}
	if out.Y0 < 0 {
		out.Y0 = 0
	}
	if out.X1 > grid.Cols-1 {
		out.X1 = grid.Cols - 1
	}
	if out.Y1 > grid.RowsN-1 {
		out.Y1 = grid.RowsN - 1
	}
	return out
}

func gridIDs(grids []*screen.Grid) []int {
	out := make([]int, 0, len(grids))
	for _, grid := range grids {
		out = append(out, grid.ID)
	}
	return out
}

func sampleFrames(grids []*screen.Grid, count int) []*screen.Grid {
	if len(grids) <= count {
		return grids
	}
	out := make([]*screen.Grid, 0, count)
	for index := 0; index < count; index++ {
		position := index * (len(grids) - 1) / (count - 1)
		out = append(out, grids[position])
	}
	return out
}

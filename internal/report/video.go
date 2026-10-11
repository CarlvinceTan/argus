package report

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/carlvincetan/argus/internal/harness"
	"github.com/carlvincetan/argus/internal/screen"
)

const (
	clipFPS       = 8
	maxClipFrames = 480
)

func findFFmpeg() (string, error) {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", fmt.Errorf("ffmpeg not found: %w", err)
	}
	return path, nil
}

// WriteTimelineClip renders the whole session as a video, choosing the newest
// grid at each tick so the recording matches what a user would have seen.
func (w *Writer) WriteTimelineClip(path string, fps int, maxFrames int) error {
	if w.FFmpegPath == "" {
		return fmt.Errorf("ffmpeg unavailable")
	}
	if len(w.gridList) < 2 {
		return fmt.Errorf("not enough frames to record")
	}
	if fps <= 0 {
		fps = clipFPS
	}
	if maxFrames <= 0 {
		maxFrames = maxClipFrames
	}
	start := w.gridList[0].Time
	end := w.gridList[len(w.gridList)-1].Time
	if end <= start {
		return fmt.Errorf("no elapsed time to record")
	}
	step := time.Second / time.Duration(fps)
	frames := []*screen.Grid{}
	for at := start; at <= end && len(frames) < maxFrames; at += step {
		frames = append(frames, gridAt(w.gridList, at))
	}
	return w.writeFramesClip(frames, path, w.Opts, screen.Region{}, color.RGBA{}, fps)
}

// WriteFindingClip renders the frames around one finding with the region
// highlighted in every frame.
func (w *Writer) WriteFindingClip(findingGrids []*screen.Grid, path string, region screen.Region, accent color.RGBA) error {
	return w.writeFramesClip(findingGrids, path, w.Opts, region, accent, clipFPS)
}

func (w *Writer) writeFramesClip(
	grids []*screen.Grid,
	path string,
	opts screen.RenderOptions,
	region screen.Region,
	accent color.RGBA,
	fps int,
) error {
	if w.FFmpegPath == "" {
		return fmt.Errorf("ffmpeg unavailable")
	}
	if len(grids) == 0 {
		return fmt.Errorf("no frames")
	}
	if fps <= 0 {
		fps = clipFPS
	}
	if len(grids) > maxClipFrames {
		grids = sampleEvery(grids, len(grids)/maxClipFrames)
	}
	tmp, err := os.MkdirTemp("", "argus-clip-*")
	if err != nil {
		return fmt.Errorf("creating frame directory: %w", err)
	}
	defer os.RemoveAll(tmp)
	for index, grid := range grids {
		var annotations []screen.Annotation
		if region.Width() > 0 && region.Height() > 0 {
			annotations = append(annotations, screen.Annotation{Region: region, Colour: accent, Filled: true})
		}
		rendered, err := screen.RenderAnnotated(grid, annotations, opts)
		if err != nil {
			return err
		}
		name := filepath.Join(tmp, fmt.Sprintf("frame-%04d.png", index))
		if err := os.WriteFile(name, rendered, 0o644); err != nil {
			return err
		}
	}
	args := []string{
		"-y",
		"-loglevel", "error",
		"-framerate", fmt.Sprintf("%d", fps),
		"-i", filepath.Join(tmp, "frame-%04d.png"),
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-pix_fmt", "yuv420p",
		"-vf", "pad=ceil(iw/2)*2:ceil(ih/2)*2",
		path,
	}
	command := exec.Command(w.FFmpegPath, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("ffmpeg failed: %w: %s", err, stderr.String())
	}
	return nil
}

// writeImageClip renders a pixel-frame sequence to a video with the finding
// region highlighted in every frame.
func (w *Writer) writeImageClip(
	frames []*harness.X11Frame,
	path string,
	region screen.Region,
	accent color.RGBA,
	fps int,
) error {
	if w.FFmpegPath == "" {
		return fmt.Errorf("ffmpeg unavailable")
	}
	if len(frames) == 0 {
		return fmt.Errorf("no frames")
	}
	if fps <= 0 {
		fps = clipFPS
	}
	tmp, err := os.MkdirTemp("", "argus-gui-clip-*")
	if err != nil {
		return fmt.Errorf("creating frame directory: %w", err)
	}
	defer os.RemoveAll(tmp)
	for index, frame := range frames {
		img := frame.Image
		if region.Width() > 0 && region.Height() > 0 {
			img = screen.AnnotateImage(frame.Image, region, accent)
		}
		name := filepath.Join(tmp, fmt.Sprintf("frame-%04d.png", index))
		if err := os.WriteFile(name, screen.EncodeImagePNG(img), 0o644); err != nil {
			return err
		}
	}
	args := []string{
		"-y", "-loglevel", "error",
		"-framerate", fmt.Sprintf("%d", fps),
		"-i", filepath.Join(tmp, "frame-%04d.png"),
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-vf", "pad=ceil(iw/2)*2:ceil(ih/2)*2",
		path,
	}
	command := exec.Command(w.FFmpegPath, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("ffmpeg failed: %w: %s", err, stderr.String())
	}
	return nil
}

func gridAt(grids []*screen.Grid, at time.Duration) *screen.Grid {
	chosen := grids[0]
	for _, grid := range grids {
		if grid.Time <= at {
			chosen = grid
			continue
		}
		break
	}
	return chosen
}

func sampleEvery(grids []*screen.Grid, step int) []*screen.Grid {
	if step < 1 {
		return grids
	}
	out := []*screen.Grid{}
	for index := 0; index < len(grids); index += step {
		out = append(out, grids[index])
	}
	if out[len(out)-1] != grids[len(grids)-1] {
		out = append(out, grids[len(grids)-1])
	}
	return out
}

// CombinePNGs is used by the visualiser to build contact sheets.
func CombinePNGs(images [][]byte) ([]byte, error) {
	decoded := make([]image.Image, 0, len(images))
	height := 0
	width := 0
	for _, raw := range images {
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		decoded = append(decoded, img)
		width += img.Bounds().Dx()
		if img.Bounds().Dy() > height {
			height = img.Bounds().Dy()
		}
	}
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	offset := 0
	for _, img := range decoded {
		draw.Draw(canvas, image.Rect(offset, 0, offset+img.Bounds().Dx(), img.Bounds().Dy()), img, img.Bounds().Min, draw.Src)
		offset += img.Bounds().Dx()
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, canvas); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

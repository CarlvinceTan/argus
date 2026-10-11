package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/carlvincetan/argus/internal/harness"
	"github.com/carlvincetan/argus/internal/jev"
	"github.com/carlvincetan/argus/internal/knowledge"
	"github.com/carlvincetan/argus/internal/provider"
	"github.com/carlvincetan/argus/internal/run"
	"github.com/carlvincetan/argus/internal/screen"
	"github.com/carlvincetan/argus/internal/sensors"
)

// GUIAuditInput is everything known about one probed GUI state.
type GUIAuditInput struct {
	Description string
	Frame       *harness.X11Frame
	Strip       []*harness.X11Frame
	Motion      sensors.MotionReportGUI
	Action      string
	History     []string
	Subject     run.Subject
	Features    []string
}

// AuditGUI analyses a GUI state.
//
// The split is the same as for terminals, with the sensors swapped: code owns
// the pixel measurements (churn, flicker, repaint, drift), the vision tier is
// asked a blinded description with a labelled tile grid for grounding, and the
// decision tier judges that description plus the measured candidates. The
// description is deliberately not "find bugs" -- it is "say what you see",
// because a model asked to confirm a hypothesis will.
func (a *Auditor) AuditGUI(ctx context.Context, in GUIAuditInput) AuditResult {
	result := AuditResult{RawAnswers: map[string]float64{}}
	if in.Frame == nil || in.Frame.Image == nil {
		result.Error = fmt.Errorf("gui audit: no frame")
		return result
	}
	grid := screen.DefaultTileGrid(in.Frame.Width, in.Frame.Height)
	result.Candidates = len(in.Motion.Evidence)
	for _, evidence := range in.Motion.Evidence {
		result.CandidateTraces = append(result.CandidateTraces, run.CandidateTrace{
			Check:      evidence.Check,
			Region:     evidence.Region,
			Detail:     evidence.Detail,
			Severity:   evidence.Severity,
			Confidence: evidence.Confidence,
			Verdict:    "candidate",
		})
	}
	description, tiles, err := a.describe(ctx, in, grid, &result)
	_ = description
	if err != nil {
		result.Error = err
		return result
	}
	directAnswers, directTiles := a.directImageDecision(ctx, in, grid, &result)
	features := in.Features
	if len(features) == 0 {
		features = inferGUIFeatures(description)
	}
	screenName := "gui:" + in.Subject.Name

	state := map[string]any{
		"app": map[string]any{
			"name":     in.Subject.Name,
			"screen":   screenName,
			"features": features,
			"window":   fmt.Sprintf("%dx%d", in.Frame.Width, in.Frame.Height),
		},
		"action":  in.Action,
		"history": in.History,
		"screen": map[string]any{
			"text":      "Blinded description of the window (vision tier):\n" + description,
			"structure": guiStructure(in.Frame, grid),
		},
		"motion": map[string]any{
			"metrics": map[string]any{
				"frames":           in.Motion.Frames,
				"duration_ms":      in.Motion.DurationMS,
				"changed_px_per_s": in.Motion.ChangedPixelsPerSec,
				"idle_change_rate": in.Motion.IdleChangedPixelRate,
			},
			"detail": motionDetailStringsGUI(in.Motion),
		},
		"candidates": candidateSummariesGUI(in.Motion.Evidence),
		"policy":     knowledge.Policy + "\n" + guiPolicy,
	}

	questions := map[string]jev.Question{}
	for id, instruction := range knowledge.ScreenQuestions() {
		switch id {
		case "defect_class", "unstated_what":
			questions[id] = jev.Choice(instruction, knowledge.ClassCriteria())
		case "severity", "unstated_severity":
			questions[id] = jev.Score(instruction, knowledge.SeverityScoreLevels())
		case "defect_row":
			// Row-based grounding does not apply to a GUI: tiles do.
			questions["defect_tile"] = jev.Choice(
				"Which labelled tile of the window contains the most visible problem? The tiles are "+grid.Describe()+".",
				tileCriteria(grid),
			)
		default:
			questions[id] = jev.Noul(instruction)
		}
	}
	for index, evidence := range in.Motion.Evidence {
		prefix := candidateID(index)
		detail := fmt.Sprintf("check=%s, region=%s (pixels), detail=%s, severity hint %.1f",
			evidence.Check, evidence.Region, evidence.Detail, evidence.Severity)
		questions[prefix+"_real_defect"] = jev.Noul(fmt.Sprintf(
			"Pixel evidence `%s` was measured on this window. Is this a real defect a user would perceive, rather than intended animation or a repaint that the application needs?", detail))
		questions[prefix+"_class"] = jev.Choice(
			fmt.Sprintf("Which defect class fits this pixel evidence? %s", detail), knowledge.ClassCriteria())
		questions[prefix+"_severity"] = jev.Score(
			fmt.Sprintf("How much does this pixel evidence affect a user? %s", detail), knowledge.SeverityScoreLevels())
	}

	repeats := a.cfg.Thresholds.TemporalRepeats
	if repeats < 1 {
		repeats = 1
	}
	started := time.Now()
	decision, err := a.providers.Decisions.Decide(ctx, state, questions, repeats, map[string]any{
		"purpose": "audit-gui",
		"screen":  screenName,
	})
	result.Latency = time.Since(started)
	if err != nil {
		result.Error = err
		return result
	}
	a.decisionCalls++
	a.inputTokens += decision.Usage.InputTokens
	a.costUSD += decision.Usage.CostUSD
	result.DecisionCalls = 1
	for id, answer := range decision.Answers {
		result.RawAnswers[id] = answer.P()
	}
	for id, answer := range directAnswers {
		current, exists := decision.Answers[id]
		if !exists || betterGUIAnswer(answer, current) {
			decision.Answers[id] = answer
			result.RawAnswers["direct_"+id] = answer.P()
		}
	}
	// If the direct image read grounded a tile and the description did not, use
	// that tile for the screen-level finding.
	if _, hasTile := decision.Answers["defect_tile"]; !hasTile {
		for label := range directTiles {
			if labels, ok := decision.Answers["defect_tile"]; ok && labels.Choice != "" {
				break
			}
			decision.Answers["defect_tile"] = jev.Answer{ID: "defect_tile", Type: "choice", Choice: label, Confidence: 0.7}
		}
	}

	added := a.guiFindings(ctx, in, grid, tiles, screenName, features, description, decision, &result)
	result.Findings = added
	return result
}

// directImageDecision is an optional second read: the Jev-like provider sees
// the screenshot itself and answers a small, calibrated question set. Its state
// contains no deterministic hypothesis (blind), and tile labels ground regions.
func (a *Auditor) directImageDecision(
	ctx context.Context,
	in GUIAuditInput,
	grid screen.TileGrid,
	result *AuditResult,
) (map[string]jev.Answer, map[string]bool) {
	imageProvider, ok := a.providers.Decisions.(provider.ImageDecisionProvider)
	if !ok || in.Frame == nil || in.Frame.Image == nil {
		return map[string]jev.Answer{}, map[string]bool{}
	}
	if a.decisionCalls >= a.cfg.Limits.MaxDecisions {
		return map[string]jev.Answer{}, map[string]bool{}
	}
	overlay := screen.OverlayTiles(in.Frame.Image, grid)
	encoded := base64.StdEncoding.EncodeToString(screen.EncodeImagePNG(overlay))
	imageURL := "data:image/png;base64," + encoded
	questions := map[string]jev.Question{
		"visible_defect": jev.Noul("Does this GUI screenshot show any user-visible defect (clipped text, overlap, unreadable text, misalignment, a broken control, or a graphic that disagrees with its label)?"),
		"defect_class":   jev.Choice("If the screenshot shows a defect, what class fits it?", knowledge.ClassCriteria()),
		"severity":       jev.Score("How much does the most visible problem in this screenshot affect a user?", knowledge.SeverityScoreLevels()),
		"defect_tile":    jev.Choice("Which labelled tile contains the most visible problem? If none, choose none.", tileCriteria(grid)),
		"unstated":       jev.Noul("Does this screenshot break an obvious UI expectation from POLICY, even though the screen does not state it?"),
	}
	state := map[string]any{
		"app":    map[string]any{"name": in.Subject.Name, "kind": "GUI"},
		"policy": knowledge.Policy + "\n" + guiPolicy,
		"tiles":  grid.Describe(),
	}
	repeats := a.cfg.Thresholds.TemporalRepeats
	if repeats < 1 {
		repeats = 1
	}
	started := time.Now()
	decision, err := imageProvider.DecideWithImages(ctx, state, questions, []string{imageURL}, repeats, map[string]any{
		"purpose": "direct-image-decision",
		"subject": in.Subject.Name,
	})
	result.Latency += time.Since(started)
	if err != nil {
		result.Rejected = append(result.Rejected, run.Finding{Title: "direct image decision failed", Detail: err.Error()})
		return map[string]jev.Answer{}, map[string]bool{}
	}
	a.decisionCalls++
	a.inputTokens += decision.Usage.InputTokens
	a.costUSD += decision.Usage.CostUSD
	result.DecisionCalls++
	answers := decision.Answers
	for id, answer := range answers {
		result.RawAnswers["direct_"+id] = answer.P()
	}
	tiles := map[string]bool{}
	if answer, ok := answers["defect_tile"]; ok && answer.Choice != "" && answer.Choice != "none" {
		tiles[answer.Choice] = true
	}
	return answers, tiles
}

func betterGUIAnswer(candidate, current jev.Answer) bool {
	if candidate.Type == "noul" {
		return candidate.Noul > current.Noul
	}
	return candidate.Confidence > current.Confidence
}

// describe asks the vision tier for a blinded description and the tiles it
// implicates.
func (a *Auditor) describe(
	ctx context.Context,
	in GUIAuditInput,
	grid screen.TileGrid,
	result *AuditResult,
) (string, map[string]bool, error) {
	if a.providers.Vision == nil {
		// Without a vision tier the GUI path still runs on pixel evidence, but
		// the description is unavailable and the audit says so.
		return "(no vision tier configured: judging pixel evidence only)", map[string]bool{}, nil
	}
	if a.visionCalls >= a.cfg.Limits.MaxVisionCalls {
		return "(vision budget exhausted)", map[string]bool{}, nil
	}
	images := [][]byte{}
	primary := grid
	annotated := screen.OverlayTiles(in.Frame.Image, grid)
	images = append(images, screen.EncodeImagePNG(annotated))
	for _, frame := range in.Strip {
		if frame == nil || frame.Image == nil {
			continue
		}
		images = append(images, screen.EncodeImagePNG(frame.Image))
		if len(images) >= 4 {
			break
		}
	}
	prompt := fmt.Sprintf(`These images show a desktop application. The first image has a labelled grid overlaid on it; the others are frames from a moment later.

Describe only what is visible:
1. What the window contains: panels, labels, lists, buttons, bars, and any text you can read.
2. Any text that is cut off mid-word, overlapping another element, hard to read against its background, or aligned inconsistently with its neighbours.
3. Any value that looks wrong on its own terms (for example a progress bar that does not match the percentage beside it).
4. For each problem you list, give the tile label (%s) where it is.

Do not speculate about the program's behaviour or intent, and do not judge whether it is good or bad. Describe, and name tiles.`,
		grid.Describe())
	description, err := a.providers.Vision.Describe(ctx, images, prompt, map[string]any{
		"purpose": "describe-gui",
		"subject": in.Subject.Name,
	})
	if err != nil {
		result.Rejected = append(result.Rejected, run.Finding{
			Title:  "vision description failed",
			Brief:  err.Error(),
			Screen: "gui:" + in.Subject.Name,
		})
		return "(vision tier failed: " + err.Error() + ")", map[string]bool{}, nil
	}
	a.visionCalls++
	tiles := map[string]bool{}
	for row := 0; row < primary.Rows; row++ {
		for col := 0; col < primary.Cols; col++ {
			label := primary.Label(col, row)
			if strings.Contains(strings.ToUpper(description), label) {
				tiles[label] = true
			}
		}
	}
	return description, tiles, nil
}

func (a *Auditor) guiFindings(
	ctx context.Context,
	in GUIAuditInput,
	grid screen.TileGrid,
	tiles map[string]bool,
	screenName string,
	features []string,
	description string,
	decision jev.Result,
	result *AuditResult,
) []run.Finding {
	added := []run.Finding{}
	feature := ""
	if len(features) > 0 {
		feature = features[0]
	}
	prob := func(id string) float64 {
		answer, ok := decision.Answers[id]
		if !ok {
			return 0
		}
		return answer.P()
	}
	pick := func(id string) (jev.Answer, bool) {
		answer, ok := decision.Answers[id]
		return answer, ok
	}
	regionFor := func(label string) screen.Region {
		if region, ok := grid.Region(label); ok {
			return region
		}
		return screen.Region{X0: 0, Y0: 0, X1: in.Frame.Width - 1, Y1: in.Frame.Height - 1}
	}

	// Vision-described defects, grounded on a tile.
	visible := prob("visible_defect")
	severityScore := 0.0
	if answer, ok := pick("severity"); ok {
		severityScore = answer.Score
	}
	class := "other"
	if answer, ok := pick("defect_class"); ok && answer.Choice != "" {
		class = answer.Choice
	}
	intended := prob("intended")
	tileRegion := screen.Region{X0: 0, Y0: 0, X1: in.Frame.Width - 1, Y1: in.Frame.Height - 1}
	if answer, ok := pick("defect_tile"); ok && answer.Choice != "" && answer.Choice != "none" {
		tileRegion = regionFor(answer.Choice)
	} else if label := firstTileMentioned(description, grid); label != "" {
		tileRegion = regionFor(label)
	}
	if visible >= a.cfg.Thresholds.DefectProb {
		finding := run.Finding{
			Title:         "window defect: " + strings.ReplaceAll(class, "_", " "),
			Brief:         firstSentences(description, 2),
			Group:         screenName,
			Screen:        screenName,
			Feature:       feature,
			Features:      features,
			Region:        tileRegion,
			Check:         "vision",
			Class:         class,
			Severity:      run.FromScore(severityScore),
			SeverityScore: severityScore,
			DefectProb:    visible,
			Confidence:    visible,
			Detail:        "raised by the blinded vision description of the window (tile " + tileLabelFor(grid, tileRegion) + ")",
			Events:        1,
			FirstSeen:     in.Frame.T,
			LastSeen:      in.Frame.T,
			GridID:        in.FrameIndex(),
			Verified:      "vision",
			ModelAnswers:  map[string]float64{"visible_defect": visible, "intended": intended},
			Tags:          []string{"vision", "gui"},
		}
		if intended >= 0.7 {
			finding.Intentional = true
			result.Rejected = append(result.Rejected, finding)
		} else {
			a.calibrate(&finding, "vision", class)
			added = append(added, a.merge(finding))
		}
	}

	// Unstated expectations in a GUI.
	if unstated := prob("unstated"); unstated >= a.cfg.Thresholds.DefectProb {
		class := "guidance_missing"
		if answer, ok := pick("unstated_what"); ok && answer.Choice != "" && answer.Choice != "none" {
			class = answer.Choice
		}
		severity := 2.0
		if answer, ok := pick("unstated_severity"); ok {
			severity = answer.Score
		}
		finding := run.Finding{
			Title:         "unstated expectation: " + strings.ReplaceAll(class, "_", " "),
			Brief:         "This window breaks an expectation users treat as obvious even though nothing states it.",
			Group:         screenName,
			Screen:        screenName,
			Feature:       feature,
			Features:      features,
			Region:        tileRegion,
			Check:         "policy",
			Class:         class,
			Severity:      run.FromScore(severity),
			SeverityScore: severity,
			DefectProb:    unstated,
			Confidence:    unstated,
			Detail:        "raised by the POLICY expectations block against the vision description",
			Events:        1,
			FirstSeen:     in.Frame.T,
			LastSeen:      in.Frame.T,
			GridID:        in.FrameIndex(),
			Verified:      "vision",
			ModelAnswers:  map[string]float64{"unstated": unstated},
			Tags:          []string{"policy", "gui"},
		}
		a.calibrate(&finding, "policy", class)
		added = append(added, a.merge(finding))
	}

	// Measured pixel evidence.
	for index, evidence := range in.Motion.Evidence {
		prefix := candidateID(index)
		real, ok := pick(prefix + "_real_defect")
		if !ok {
			continue
		}
		class := knowledge.ClassFromCheck(evidence.Check)
		if answer, ok := pick(prefix + "_class"); ok && answer.Choice != "" && answer.Choice != "none" {
			class = answer.Choice
		}
		severityScore := evidence.Severity
		if answer, ok := pick(prefix + "_severity"); ok {
			severityScore = answer.Score
		}
		finding := run.Finding{
			Title:         guiCheckName(evidence.Check),
			Brief:         evidence.Detail,
			Group:         screenName,
			Screen:        screenName,
			Feature:       feature,
			Features:      features,
			Region:        evidence.Region,
			Check:         evidence.Check,
			Class:         class,
			Severity:      run.FromScore(severityScore),
			SeverityScore: severityScore,
			DefectProb:    real.P(),
			Confidence:    combineConfidence(real.P(), evidence.Confidence),
			Detail:        evidence.Detail,
			Measured:      evidence.Measured,
			Events:        1,
			FirstSeen:     in.Frame.T,
			LastSeen:      in.Frame.T,
			GridID:        in.FrameIndex(),
			Verified:      "jev",
			ModelAnswers:  map[string]float64{prefix + "_defect": real.P()},
			Tags:          []string{"motion", "gui", evidence.Check},
		}
		if adjusted, ok := measuredCheckProbability(evidence.Check, evidence.Confidence, real.P()); ok {
			finding.DefectProb = adjusted
		} else {
			result.Rejected = append(result.Rejected, finding)
			markCandidate(result, evidence.Check, evidence.Region, "model called it a non-issue")
			continue
		}
		markCandidate(result, evidence.Check, evidence.Region, fmt.Sprintf("model defect probability %.2f", real.P()))
		a.calibrate(&finding, evidence.Check, class)
		if real.P() < a.cfg.Thresholds.DefectProb {
			result.Rejected = append(result.Rejected, finding)
			continue
		}
		added = append(added, a.merge(finding))
	}
	return added
}

// FrameIndex exposes the frame's ledger position for evidence lookup.
func (in GUIAuditInput) FrameIndex() int { return int(in.Frame.T / (100 * time.Millisecond)) }

const guiPolicy = `GUI ADDITIONS
- A window is a composition: overlapping text and unlabelled controls are defects, and a control that looks
  disabled but is clickable (or the reverse) is an inconsistency.
- A value that disagrees with its own graphic (a bar filled 40% beside "90%") is a defect regardless of layout.
- A window that repaints while nothing is happening costs battery and causes flicker; so does a region that
  blinks without conveying state.`

func tileCriteria(grid screen.TileGrid) map[string]string {
	criteria := map[string]string{"none": "no single tile stands out"}
	for row := 0; row < grid.Rows; row++ {
		for col := 0; col < grid.Cols; col++ {
			criteria[grid.Label(col, row)] = fmt.Sprintf("tile %s (x %d-%d, y %d-%d)",
				grid.Label(col, row),
				col*grid.Width/grid.Cols, (col+1)*grid.Width/grid.Cols-1,
				row*grid.Height/grid.Rows, (row+1)*grid.Height/grid.Rows-1)
		}
	}
	return criteria
}

// firstTileMentioned finds the first tile label the description names, which
// grounds a finding when the model declined the explicit tile question.
func firstTileMentioned(description string, grid screen.TileGrid) string {
	upper := strings.ToUpper(description)
	best, bestIndex := "", -1
	for row := 0; row < grid.Rows; row++ {
		for col := 0; col < grid.Cols; col++ {
			label := grid.Label(col, row)
			index := strings.Index(upper, label)
			if index >= 0 && (bestIndex < 0 || index < bestIndex) {
				best, bestIndex = label, index
			}
		}
	}
	return best
}

func tileLabelFor(grid screen.TileGrid, region screen.Region) string {
	centreX := (region.X0 + region.X1) / 2
	centreY := (region.Y0 + region.Y1) / 2
	col := centreX * grid.Cols / maxInt(grid.Width, 1)
	row := centreY * grid.Rows / maxInt(grid.Height, 1)
	if col >= grid.Cols {
		col = grid.Cols - 1
	}
	if row >= grid.Rows {
		row = grid.Rows - 1
	}
	return grid.Label(col, row)
}

func guiStructure(frame *harness.X11Frame, grid screen.TileGrid) string {
	return fmt.Sprintf("window %dx%d pixels, tile grid %dx%d; measured change: %d of %d pixels differ from the previous frame (%.1f%%)",
		frame.Width, frame.Height, grid.Cols, grid.Rows,
		frame.ChangedPixels, frame.TotalPixels,
		100*float64(frame.ChangedPixels)/float64(maxInt(frame.TotalPixels, 1)))
}

func candidateSummariesGUI(evidence []sensors.Evidence) []string {
	out := make([]string, 0, len(evidence))
	for index, item := range evidence {
		out = append(out, fmt.Sprintf("%s: check=%s region=%s detail=%s", candidateID(index), item.Check, item.Region, item.Detail))
	}
	return out
}

func motionDetailStringsGUI(report sensors.MotionReportGUI) []string {
	out := make([]string, 0, len(report.Evidence))
	for _, item := range report.Evidence {
		out = append(out, fmt.Sprintf("%s at %s: %s", item.Check, item.Region, item.Detail))
	}
	return out
}

func guiCheckName(check string) string {
	switch check {
	case sensors.GUIFlicker:
		return "flicker in a window region"
	case sensors.GUIFullRepaint:
		return "whole window repaints in one frame"
	case sensors.GUILayoutDrift:
		return "content drifts while idle"
	case sensors.GUIIdleChurn:
		return "window repaints while idle"
	default:
		return strings.ReplaceAll(check, "_", " ")
	}
}

// inferGUIFeatures pulls a few feature-ish phrases out of a description so the
// report can group GUI findings.
func inferGUIFeatures(description string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, sentence := range strings.Split(description, ".") {
		sentence = strings.TrimSpace(sentence)
		if len(sentence) < 4 || len(sentence) > 60 {
			continue
		}
		if strings.ContainsAny(sentence, "0123456789") && !strings.Contains(sentence, " ") {
			continue
		}
		key := strings.ToLower(sentence)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, sentence)
		if len(out) >= 4 {
			break
		}
	}
	return out
}

func firstSentences(text string, count int) string {
	trimmed := strings.TrimSpace(text)
	sentences := strings.Split(trimmed, ".")
	if len(sentences) <= count {
		return trimTo(trimmed, 300)
	}
	return trimTo(strings.Join(sentences[:count], ".")+".", 300)
}

func trimTo(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}

package webui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/run"
	"github.com/carlvincetan/argus/internal/triage"
)

// RunSummary is what the sidebar needs: enough to pick a run without loading it.
type RunSummary struct {
	Path          string         `json:"path"`
	ID            string         `json:"id"`
	Subject       string         `json:"subject"`
	Version       string         `json:"version,omitempty"`
	Mode          string         `json:"mode"`
	Provider      string         `json:"provider"`
	DecisionModel string         `json:"decisionModel"`
	VisionModel   string         `json:"visionModel,omitempty"`
	StartedAt     time.Time      `json:"startedAt"`
	DurationMS    int64          `json:"durationMs"`
	Terminal      string         `json:"terminal"`
	Reported      int            `json:"reported"`
	Total         int            `json:"total"`
	Rejected      int            `json:"rejected"`
	CostUSD       float64        `json:"costUsd"`
	Severities    map[string]int `json:"severities"`
}

// EvidenceSet holds the browser URLs of one finding's evidence.
type EvidenceSet struct {
	Still string `json:"still,omitempty"`
	Crop  string `json:"crop,omitempty"`
	Strip string `json:"strip,omitempty"`
	Clip  string `json:"clip,omitempty"`
}

// CandidateRow is a deterministic candidate with the model's verdict, kept so a
// reviewer can see what was considered and dropped.
type CandidateRow struct {
	Observation string      `json:"observation"`
	Screen      string      `json:"screen"`
	Action      string      `json:"action"`
	Check       string      `json:"check"`
	Region      string      `json:"region"`
	Detail      string      `json:"detail"`
	Severity    float64     `json:"severityHint"`
	Verdict     string      `json:"verdict"`
	Evidence    run.Finding `json:"-"`
	RegionXY    [4]int      `json:"regionXY"`
}

// RunDetail is everything the main view renders for one run.
type RunDetail struct {
	Summary       RunSummary        `json:"summary"`
	Findings      []RunFinding      `json:"findings"`
	Rejected      []RunFinding      `json:"rejected"`
	Candidates    []CandidateRow    `json:"candidates"`
	SessionClip   string            `json:"sessionClip,omitempty"`
	Isolation     map[string]any    `json:"isolation,omitempty"`
	Notes         []string          `json:"notes"`
	Log           []string          `json:"log"`
	GroundTruth   map[string]string `json:"groundTruth,omitempty"`
	Observations  int               `json:"observations"`
	FrameCount    int               `json:"frameCount"`
	DecisionCalls int               `json:"decisionCalls"`
	VisionCalls   int               `json:"visionCalls"`
}

// RunFinding is a finding enriched with browser URLs.
type RunFinding struct {
	run.Finding
	Evidence EvidenceSet `json:"evidence"`
}

// LabelRequest is one reviewer decision coming from the UI.
type LabelRequest struct {
	RunPath   string `json:"runPath"`
	FindingID string `json:"findingId"`
	Verdict   string `json:"verdict"`
	Note      string `json:"note"`
	Reviewer  string `json:"reviewer"`
}

// CalibrationView is the learned state, shown in the UI so the effect of
// labelling is visible rather than mysterious.
type CalibrationView struct {
	Summary    string                      `json:"summary"`
	Labels     int                         `json:"labels"`
	Generated  time.Time                   `json:"generatedAt"`
	Suppressed []string                    `json:"suppressed"`
	Promoted   []string                    `json:"promoted"`
	Checks     map[string]triage.CheckStat `json:"checks"`
	History    triage.Report               `json:"history"`
	LabelPath  string                      `json:"labelPath"`
}

// Service backs the web UI and any programmatic client (the CLI included).
type Service struct {
	mu     sync.Mutex
	dirs   map[string]string // run id -> directory
	labels *triage.Store
	config config.Config

	// runs tracks sessions started from the web UI, so a remote user can start
	// and watch an audit without shell access.
	runs        map[string]*launchedRun
	seq         int
	argusBinary string
}

// NewRunService creates the service.
func NewService() *Service {
	cfg := config.Default()
	if err := cfg.Load(); err != nil {
		cfg = config.Default()
	}
	store, err := triage.Open(cfg.LabelPath)
	if err != nil {
		store = nil
	}
	return &Service{
		dirs:   map[string]string{},
		labels: store,
		config: cfg,
		runs:   map[string]*launchedRun{},
	}
}

// Label records a reviewer verdict for one finding and returns the updated view.
func (s *Service) Label(request LabelRequest) (CalibrationView, error) {
	if s.labels == nil {
		return CalibrationView{}, fmt.Errorf("label store unavailable")
	}
	if !triage.Valid(request.Verdict) {
		return CalibrationView{}, fmt.Errorf("unknown verdict %q", request.Verdict)
	}
	session, err := readSession(resolveRunPath(request.RunPath))
	if err != nil {
		return CalibrationView{}, err
	}
	var target *run.Finding
	needle := strings.ToLower(request.FindingID)
	for index := range session.Findings {
		finding := &session.Findings[index]
		if finding.ID == request.FindingID || strings.Contains(strings.ToLower(finding.Title), needle) {
			target = finding
			break
		}
	}
	if target == nil {
		return CalibrationView{}, fmt.Errorf("finding %q not found in this run", request.FindingID)
	}
	label := triage.Label{
		FindingID:    target.ID,
		RunID:        session.ID,
		Subject:      session.Subject.Name,
		Kind:         session.Kind,
		Check:        target.Check,
		Class:        target.Class,
		Group:        target.Group,
		Severity:     target.SeverityName,
		Verified:     target.Verified,
		Title:        target.Title,
		DefectProb:   target.DefectProb,
		ModelAnswers: target.ModelAnswers,
		Verdict:      triage.Verdict(request.Verdict),
		Note:         request.Note,
		Reviewer:     request.Reviewer,
		At:           time.Now().UTC(),
	}
	if err := s.labels.Append(label); err != nil {
		return CalibrationView{}, err
	}
	return s.Calibration()
}

// Calibration returns the learned state plus the label history summary.
func (s *Service) Calibration() (CalibrationView, error) {
	view := CalibrationView{
		Summary:    "none",
		Checks:     map[string]triage.CheckStat{},
		Suppressed: []string{},
		Promoted:   []string{},
	}
	calibration, err := triage.Load(s.config.CalibrationPath)
	if err != nil {
		return view, err
	}
	view.Summary = calibration.Summary()
	view.Labels = calibration.Labels
	view.Generated = calibration.GeneratedAt
	view.Suppressed = calibration.Suppressed
	view.Promoted = calibration.Promoted
	if calibration.Checks != nil {
		view.Checks = calibration.Checks
	}
	if s.labels != nil {
		labels, err := s.labels.All()
		if err == nil {
			view.History = triage.Summarise(labels)
		}
		view.LabelPath = s.labels.Path()
	}
	return view, nil
}

// Recalibrate rebuilds the calibration from the label history.
func (s *Service) Recalibrate() (CalibrationView, error) {
	if s.labels == nil {
		return CalibrationView{}, fmt.Errorf("label store unavailable")
	}
	labels, err := s.labels.All()
	if err != nil {
		return CalibrationView{}, err
	}
	calibration := triage.Build(labels, triage.DefaultOptions())
	if err := calibration.Save(s.config.CalibrationPath); err != nil {
		return CalibrationView{}, err
	}
	return s.Calibration()
}

// LabelsFor returns the latest verdict per finding for one run.
func (s *Service) LabelsFor(runPath string) (map[string]string, error) {
	out := map[string]string{}
	if s.labels == nil {
		return out, nil
	}
	session, err := readSession(resolveRunPath(runPath))
	if err != nil {
		return out, err
	}
	labels, err := s.labels.ByFinding()
	if err != nil {
		return out, err
	}
	for _, finding := range session.Findings {
		if label, ok := labels[finding.ID]; ok {
			out[finding.ID] = string(label.Verdict)
		}
	}
	return out, nil
}

func resolveRunPath(path string) string {
	if strings.HasSuffix(path, "run.json") {
		return path
	}
	return filepath.Join(path, "run.json")
}

// Roots lists the directories Argus writes runs into, so the app can offer them
// without the user typing paths.
func (s *Service) Roots() []string {
	candidates := []string{
		"var/runs",
		"benchmark/results",
	}
	out := []string{}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			absolute, err := filepath.Abs(candidate)
			if err == nil {
				out = append(out, absolute)
			}
		}
	}
	return out
}

// ListRuns scans a directory tree (depth 3) for run.json files.
func (s *Service) ListRuns(root string) ([]RunSummary, error) {
	if strings.TrimSpace(root) == "" {
		roots := s.Roots()
		if len(roots) == 0 {
			return []RunSummary{}, nil
		}
		summaries := []RunSummary{}
		for _, candidate := range roots {
			found, err := s.ListRuns(candidate)
			if err == nil {
				summaries = append(summaries, found...)
			}
		}
		sortSummaries(summaries)
		return summaries, nil
	}
	found := []RunSummary{}
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if depth(root, path) > 3 {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "run.json" {
			return nil
		}
		summary, err := s.summarise(path)
		if err == nil {
			found = append(found, summary)
		}
		return nil
	})
	sortSummaries(found)
	return found, nil
}

func depth(root, path string) int {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return 0
	}
	if relative == "." {
		return 0
	}
	return strings.Count(relative, string(os.PathSeparator)) + 1
}

func sortSummaries(summaries []RunSummary) {
	sort.SliceStable(summaries, func(i, j int) bool {
		return summaries[i].StartedAt.After(summaries[j].StartedAt)
	})
}

func (s *Service) summarise(path string) (RunSummary, error) {
	session, err := readSession(path)
	if err != nil {
		return RunSummary{}, err
	}
	directory := filepath.Dir(path)
	absolute, err := filepath.Abs(directory)
	if err != nil {
		absolute = directory
	}
	s.mu.Lock()
	s.dirs[session.ID] = absolute
	s.mu.Unlock()

	reported := 0
	severities := map[string]int{}
	for _, finding := range session.Findings {
		if finding.Reported {
			reported++
			severities[finding.SeverityName]++
		}
	}
	return RunSummary{
		Path:          absolute,
		ID:            session.ID,
		Subject:       session.Subject.Name,
		Version:       session.Subject.Version,
		Mode:          session.Mode,
		Provider:      session.Manifest.Provider,
		DecisionModel: session.Manifest.DecisionModel,
		VisionModel:   session.Manifest.VisionModel,
		StartedAt:     session.StartedAt,
		DurationMS:    session.Duration.Milliseconds(),
		Terminal:      fmt.Sprintf("%dx%d", session.Terminal[0], session.Terminal[1]),
		Reported:      reported,
		Total:         len(session.Findings),
		Rejected:      len(session.Rejected),
		CostUSD:       session.Cost.CostUSD,
		Severities:    severities,
	}, nil
}

// LoadRun loads one run, including evidence URLs.
func (s *Service) LoadRun(path string) (RunDetail, error) {
	if strings.TrimSpace(path) == "" {
		return RunDetail{}, fmt.Errorf("no run selected")
	}
	if !strings.HasSuffix(path, "run.json") {
		path = filepath.Join(path, "run.json")
	}
	directory := filepath.Dir(path)
	absolute, err := filepath.Abs(directory)
	if err != nil {
		absolute = directory
	}
	session, err := readSession(path)
	if err != nil {
		return RunDetail{}, err
	}
	s.mu.Lock()
	s.dirs[session.ID] = absolute
	s.mu.Unlock()

	summary, err := s.summarise(path)
	if err != nil {
		return RunDetail{}, err
	}

	detail := RunDetail{
		Summary:       summary,
		Findings:      []RunFinding{},
		Rejected:      []RunFinding{},
		Candidates:    []CandidateRow{},
		Isolation:     session.Isolation,
		Notes:         session.Notes,
		Log:           session.Log,
		Observations:  len(session.Observations),
		FrameCount:    session.Stats.Frames,
		DecisionCalls: session.Cost.DecisionCalls,
		VisionCalls:   session.Cost.VisionCalls,
	}
	for _, finding := range session.Findings {
		detail.Findings = append(detail.Findings, RunFinding{Finding: finding, Evidence: s.evidence(session.ID, finding)})
	}
	for _, finding := range session.Rejected {
		detail.Rejected = append(detail.Rejected, RunFinding{Finding: finding, Evidence: s.evidence(session.ID, finding)})
	}
	for _, observation := range session.Observations {
		for _, candidate := range observation.Candidates {
			detail.Candidates = append(detail.Candidates, CandidateRow{
				Observation: observation.ID,
				Screen:      observation.Screen,
				Action:      observation.Action,
				Check:       candidate.Check,
				Region:      candidate.Region.String(),
				RegionXY:    [4]int{candidate.Region.X0, candidate.Region.Y0, candidate.Region.X1, candidate.Region.Y1},
				Detail:      candidate.Detail,
				Severity:    candidate.Severity,
				Verdict:     candidate.Verdict,
			})
		}
	}
	if _, err := os.Stat(filepath.Join(absolute, "clips", "session.mp4")); err == nil {
		detail.SessionClip = "/evidence/" + session.ID + "/clips/session.mp4"
	}
	return detail, nil
}

func (s *Service) evidence(runID string, finding run.Finding) EvidenceSet {
	set := EvidenceSet{}
	url := func(relative string) string {
		if strings.TrimSpace(relative) == "" {
			return ""
		}
		return "/evidence/" + runID + "/" + filepath.ToSlash(relative)
	}
	set.Still = url(finding.Still)
	set.Crop = url(finding.Crop)
	set.Strip = url(finding.Strip)
	set.Clip = url(finding.Clip)
	return set
}

// RunDir exposes the directory for a run so the asset handler can serve files.
func (s *Service) RunDir(runID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	directory, ok := s.dirs[runID]
	return directory, ok
}

func readSession(path string) (*run.Session, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var session run.Session
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &session, nil
}

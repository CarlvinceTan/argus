// Package run holds the domain model that the pipeline produces and the
// visualiser consumes: observations, findings, severity and the session record.
//
// A Finding is the unit of review. It always carries the evidence that produced
// it (a screen region, the check or model answer that flagged it) so a human can
// confirm or reject it in seconds.
package run

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/carlvincetan/argus/internal/audit"
	"github.com/carlvincetan/argus/internal/provider"
	"github.com/carlvincetan/argus/internal/screen"
)

// Severity is the code-owned impact scale, matching the Jev severity legend.
type Severity int

// Severity levels, ordered from least to most severe.
const (
	SevCosmetic Severity = iota
	SevMinor
	SevNoticeable
	SevMajor
	SevCritical
)

var severityNames = []string{"cosmetic", "minor", "noticeable", "major", "critical"}

func (s Severity) String() string {
	if int(s) < 0 || int(s) >= len(severityNames) {
		return "unknown"
	}
	return severityNames[s]
}

// FromScore converts a probability-weighted severity score to a level.
func FromScore(score float64) Severity {
	switch {
	case score >= 3.5:
		return SevCritical
	case score >= 2.5:
		return SevMajor
	case score >= 1.5:
		return SevNoticeable
	case score >= 0.5:
		return SevMinor
	default:
		return SevCosmetic
	}
}

// Subject kinds.
const (
	KindTUI = "tui"
	KindGUI = "gui"
)

// Subject describes what was tested and how.
type Subject struct {
	Name     string            `json:"name"`
	Argv     []string          `json:"argv"`
	Version  string            `json:"version,omitempty"`
	Commit   string            `json:"commit,omitempty"`
	Notes    string            `json:"notes,omitempty"`
	Tags     []string          `json:"tags,omitempty"`
	WorkDir  string            `json:"work_dir,omitempty"`
	Terminal string            `json:"terminal,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

// Observation is one probed state of the UI.
type Observation struct {
	ID         string           `json:"id"`
	T          time.Duration    `json:"t_ms"`
	GridID     int              `json:"grid_id"`
	Screen     string           `json:"screen"`
	Features   []string         `json:"features"`
	Action     string           `json:"action,omitempty"`
	Evidence   []string         `json:"evidence_ids"`
	Still      string           `json:"still,omitempty"`
	Note       string           `json:"note,omitempty"`
	ModelCalls int              `json:"model_calls"`
	Cost       float64          `json:"cost_usd"`
	Candidates []CandidateTrace `json:"candidates,omitempty"`
	Error      string           `json:"error,omitempty"`
}

// CandidateTrace records what the deterministic tier saw and what the model
// decided about it, including candidates that never became findings. A backlog
// nobody can audit is not evidence, so both directions are kept.
type CandidateTrace struct {
	Check      string        `json:"check"`
	Region     screen.Region `json:"region"`
	Detail     string        `json:"detail"`
	Snippet    string        `json:"snippet,omitempty"`
	Severity   float64       `json:"severity_hint"`
	Confidence float64       `json:"confidence"`
	DefectProb float64       `json:"defect_prob,omitempty"`
	Verdict    string        `json:"verdict"`
}

// Finding is one reported issue.
type Finding struct {
	ID            string             `json:"id"`
	Title         string             `json:"title"`
	Brief         string             `json:"brief"`
	Group         string             `json:"group"`
	Screen        string             `json:"screen"`
	Feature       string             `json:"feature,omitempty"`
	Features      []string           `json:"features"`
	Region        screen.Region      `json:"region"`
	Check         string             `json:"check,omitempty"`
	Class         string             `json:"class"`
	Severity      Severity           `json:"severity"`
	SeverityScore float64            `json:"severity_score"`
	SeverityName  string             `json:"severity_name"`
	DefectProb    float64            `json:"defect_prob"`
	Confidence    float64            `json:"confidence"`
	Intentional   bool               `json:"intentional"`
	Reported      bool               `json:"reported"`
	Verified      string             `json:"verified"` // "deterministic", "jev", "vision", "unverified"
	Detail        string             `json:"detail,omitempty"`
	Measured      float64            `json:"measured,omitempty"`
	Events        int                `json:"events"`
	FirstSeen     time.Duration      `json:"first_seen_ms"`
	LastSeen      time.Duration      `json:"last_seen_ms"`
	GridID        int                `json:"grid_id"`
	Still         string             `json:"still,omitempty"`
	Crop          string             `json:"crop,omitempty"`
	Strip         string             `json:"strip,omitempty"`
	Clip          string             `json:"clip,omitempty"`
	Frames        []int              `json:"frames,omitempty"`
	ModelAnswers  map[string]float64 `json:"model_answers,omitempty"`
	Tags          []string           `json:"tags,omitempty"`
}

// Stats summarises a session.
type Stats struct {
	Observations int            `json:"observations"`
	Screens      []string       `json:"screens"`
	Actions      int            `json:"actions"`
	Frames       int            `json:"frames"`
	Duration     time.Duration  `json:"duration"`
	Candidates   int            `json:"candidates"`
	Findings     int            `json:"findings"`
	BySeverity   map[string]int `json:"by_severity"`
	ByGroup      map[string]int `json:"by_group"`
}

// Cost records model usage so a run can be judged on cost as well as quality.
type Cost struct {
	DecisionCalls int     `json:"decision_calls"`
	VisionCalls   int     `json:"vision_calls"`
	InputTokens   int     `json:"input_tokens"`
	CostUSD       float64 `json:"cost_usd"`
	Local         bool    `json:"local"`
}

// Session is the complete record of one testing run.
type Session struct {
	ID        string            `json:"id"`
	Subject   Subject           `json:"subject"`
	Mode      string            `json:"mode"`
	Kind      string            `json:"kind"`
	Manifest  provider.Manifest `json:"manifest"`
	StartedAt time.Time         `json:"started_at"`
	Duration  time.Duration     `json:"duration_ms"`
	Terminal  [2]int            `json:"terminal"`
	Stats     Stats             `json:"stats"`
	Cost      Cost              `json:"cost"`
	// Inventory is what the auditor measured the application to offer: features,
	// screens, lists and structure. Kept in the run record so a completeness
	// finding can be argued with, not just believed.
	Inventory *audit.Inventory `json:"inventory,omitempty"`

	Observations []Observation  `json:"observations"`
	Findings     []Finding      `json:"findings"`
	Rejected     []Finding      `json:"rejected"`
	Log          []string       `json:"log"`
	Notes        []string       `json:"notes"`
	Isolation    map[string]any `json:"isolation,omitempty"`
}

// NewSession builds an empty session for a subject.
func NewSession(subject Subject, manifest provider.Manifest, mode string, cols, rows int) *Session {
	fingerprint := sha256.Sum256([]byte(strings.Join(subject.Argv, " ") + subject.Version + subject.Commit))
	return &Session{
		ID:        hex.EncodeToString(fingerprint[:6]),
		Subject:   subject,
		Mode:      mode,
		Kind:      KindTUI,
		Manifest:  manifest,
		StartedAt: time.Now(),
		Terminal:  [2]int{cols, rows},
		Stats: Stats{
			Screens:    []string{},
			BySeverity: map[string]int{},
			ByGroup:    map[string]int{},
		},
		Observations: []Observation{},
		Findings:     []Finding{},
		Rejected:     []Finding{},
		Log:          []string{},
		Notes:        []string{},
	}
}

// AddLog appends a timestamped line to the session log.
func (s *Session) AddLog(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	s.Log = append(s.Log, fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), line))
}

// AddNote records a human-readable caveat about the run.
func (s *Session) AddNote(note string) {
	s.Notes = append(s.Notes, note)
}

// Finalise recomputes derived stats and sorts findings by severity.
func (s *Session) Finalise(duration time.Duration, frames, actions int) {
	s.Duration = duration
	s.Stats.Observations = len(s.Observations)
	s.Stats.Findings = len(s.Findings)
	s.Stats.Frames = frames
	s.Stats.Actions = actions
	s.Stats.Duration = duration
	s.Stats.BySeverity = map[string]int{}
	s.Stats.ByGroup = map[string]int{}
	screens := map[string]bool{}
	for _, finding := range s.Findings {
		s.Stats.BySeverity[finding.SeverityName]++
		if finding.Group != "" {
			s.Stats.ByGroup[finding.Group]++
		}
		if finding.Screen != "" {
			screens[finding.Screen] = true
		}
	}
	s.Stats.Screens = s.Stats.Screens[:0]
	for name := range screens {
		s.Stats.Screens = append(s.Stats.Screens, name)
	}
	sort.Strings(s.Stats.Screens)
	SortFindings(s.Findings)
	ApplyReportingPolicy(s)
}

// Tags linking a finding to the app's intent ledger.
const (
	TagIntent      = "intent:"
	TagStaleIntent = "intent_stale:"
)

// MaxReportedPerGroup keeps a backlog readable: a page with fifty findings is a
// page nobody triages. Everything beyond the cap stays in run.json.
const MaxReportedPerGroup = 5

// findingPriority orders findings the way a triage backlog should be filled:
// anything code measured outranks anything a model speculated, and motion
// evidence outranks a screen-wide impression.
func findingPriority(f Finding) int {
	switch {
	case f.Check == "screen":
		return 1
	case strings.HasPrefix(f.Check, "policy"):
		return 0
	case f.Class == "animation" || f.Class == "layout_jitter" || f.Class == "flicker":
		return 3
	default:
		return 2
	}
}

// ApplyReportingPolicy collapses screen-wide findings that a specific check
// already covers, then caps how many findings each page contributes to the
// backlog (measured evidence first, then severity, because findings arrive
// pre-sorted by severity).
func ApplyReportingPolicy(s *Session) {
	specific := map[string]bool{}
	for _, finding := range s.Findings {
		if !finding.Reported || finding.Check == "screen" || strings.HasPrefix(finding.Check, "policy") {
			continue
		}
		specific[finding.Group+"|"+finding.Class] = true
	}
	// A specific finding of the same class supersedes a screen-wide one.
	type key struct{ group, class string }
	covered := map[key]bool{}
	for index := range s.Findings {
		finding := &s.Findings[index]
		if !finding.Reported || finding.Check == "screen" || strings.HasPrefix(finding.Check, "policy") {
			continue
		}
		covered[key{finding.Group, finding.Class}] = true
	}
	collapsed, capped := 0, 0
	ranked := append([]Finding{}, s.Findings...)
	sort.SliceStable(ranked, func(i, j int) bool {
		pi, pj := findingPriority(ranked[i]), findingPriority(ranked[j])
		if pi != pj {
			return pi > pj
		}
		if ranked[i].Severity != ranked[j].Severity {
			return ranked[i].Severity > ranked[j].Severity
		}
		return ranked[i].DefectProb > ranked[j].DefectProb
	})
	perGroup := map[string]int{}
	for _, candidate := range ranked {
		finding := &s.Findings[findIndex(s.Findings, candidate.ID)]
		// The cap limits speculation; a measured fact isn't noise however many there are.
		if !finding.Reported || finding.Verified == "deterministic" {
			continue
		}
		if (finding.Check == "screen" || strings.HasPrefix(finding.Check, "policy")) && covered[key{finding.Group, finding.Class}] {
			finding.Reported = false
			finding.Tags = append(finding.Tags, "covered_by_specific_check")
			collapsed++
			continue
		}
		perGroup[finding.Group]++
		if perGroup[finding.Group] > MaxReportedPerGroup {
			finding.Reported = false
			finding.Tags = append(finding.Tags, "capped_per_page")
			capped++
		}
	}
	if collapsed > 0 {
		s.AddNote(fmt.Sprintf("%d screen-wide findings were folded into a specific finding of the same class on the same page", collapsed))
	}
	if capped > 0 {
		s.AddNote(fmt.Sprintf("%d findings were moved below the reporting threshold by the per-page cap of %d", capped, MaxReportedPerGroup))
	}
}

// findIndex returns the position of a finding by id.
func findIndex(findings []Finding, id string) int {
	for index := range findings {
		if findings[index].ID == id {
			return index
		}
	}
	return 0
}

// SortFindings orders findings the way a triage backlog should be read: most
// severe first, then most likely, then earliest seen.
func SortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Severity != b.Severity {
			return a.Severity > b.Severity
		}
		if a.DefectProb != b.DefectProb {
			return a.DefectProb > b.DefectProb
		}
		return a.FirstSeen < b.FirstSeen
	})
}

// GroupFindings buckets findings by their page/feature group, preserving the
// severity ordering inside each bucket.
func GroupFindings(findings []Finding) map[string][]Finding {
	groups := map[string][]Finding{}
	for _, finding := range findings {
		key := finding.Group
		if key == "" {
			key = "unlabelled"
		}
		groups[key] = append(groups[key], finding)
	}
	return groups
}

// FindingID builds a stable identifier so triage labels survive across runs.
// The region is bucketed because coordinates shift when the terminal is resized
// or the content scrolls; the bucket keeps the same logical defect together.
func FindingID(check, group, class string, region screen.Region) string {
	anchor := fmt.Sprintf("%s|%s|%s|%d|%d", check, group, class, region.Y0/6, region.X0/12)
	sum := sha256.Sum256([]byte(anchor))
	return hex.EncodeToString(sum[:6])
}

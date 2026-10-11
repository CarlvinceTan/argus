// Package triage stores human verdicts on findings and turns them into a
// calibration model the pipeline consumes.
//
// The point is to close the loop: every finding can be labelled (accepted,
// rejected, misclassified, intentional, duplicate) with a free-text reason, the
// labels accumulate in an append-only JSONL log, and a *data* file is derived
// from them. Loading that data file changes how the pipeline weighs evidence --
// no code change, no prompt edit, and it can be reset or regenerated at any
// time. Because the labels keep the original features (check, class, model
// answers), a better model can be refit later from the same history.
package triage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Verdict is a reviewer's decision about one finding.
type Verdict string

// The verdicts a reviewer can record.
const (
	Accepted      Verdict = "accepted"
	Rejected      Verdict = "rejected"
	Misclassified Verdict = "misclassified"
	Intentional   Verdict = "intentional"
	Duplicate     Verdict = "duplicate"
)

// Verdicts lists the valid verdicts in display order.
func Verdicts() []Verdict {
	return []Verdict{Accepted, Rejected, Misclassified, Intentional, Duplicate}
}

// Valid reports whether a verdict string is recognised.
func Valid(value string) bool {
	for _, verdict := range Verdicts() {
		if string(verdict) == value {
			return true
		}
	}
	return false
}

// Label is one reviewer decision plus the finding features it refers to.
type Label struct {
	FindingID    string             `json:"finding_id"`
	RunID        string             `json:"run_id"`
	Subject      string             `json:"subject"`
	Kind         string             `json:"kind,omitempty"`
	Check        string             `json:"check,omitempty"`
	Class        string             `json:"class,omitempty"`
	Group        string             `json:"group,omitempty"`
	Severity     string             `json:"severity,omitempty"`
	Verified     string             `json:"verified,omitempty"`
	Title        string             `json:"title,omitempty"`
	DefectProb   float64            `json:"defect_prob"`
	ModelAnswers map[string]float64 `json:"model_answers,omitempty"`
	Verdict      Verdict            `json:"verdict"`
	Note         string             `json:"note,omitempty"`
	Reviewer     string             `json:"reviewer,omitempty"`
	At           time.Time          `json:"at"`
}

// Store is an append-only label log.
type Store struct {
	path string
	mu   sync.Mutex
}

// Open prepares a label store at the given path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating label directory: %w", err)
	}
	return &Store{path: path}, nil
}

// Path returns the label log location.
func (s *Store) Path() string { return s.path }

// Append writes one label. Appending never rewrites history, so the log stays
// auditable and a relabelling is just another line.
func (s *Store) Append(label Label) error {
	if !Valid(string(label.Verdict)) {
		return fmt.Errorf("unknown verdict %q", label.Verdict)
	}
	if label.At.IsZero() {
		label.At = time.Now().UTC()
	}
	if label.FindingID == "" {
		return fmt.Errorf("label needs a finding id")
	}
	encoded, err := json.Marshal(label)
	if err != nil {
		return fmt.Errorf("encoding label: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening label log: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("writing label: %w", err)
	}
	return nil
}

// All reads every label, with the latest verdict per finding winning.
func (s *Store) All() ([]Label, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Label{}, nil
		}
		return nil, fmt.Errorf("opening label log: %w", err)
	}
	defer file.Close()

	latest := map[string]Label{}
	order := []string{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var label Label
		if err := json.Unmarshal(line, &label); err != nil {
			continue // a corrupt line must not lose the rest of the history
		}
		if _, seen := latest[label.FindingID]; !seen {
			order = append(order, label.FindingID)
		}
		latest[label.FindingID] = label
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading label log: %w", err)
	}
	out := make([]Label, 0, len(order))
	for _, id := range order {
		out = append(out, latest[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// ByFinding returns the latest label for each finding id.
func (s *Store) ByFinding() (map[string]Label, error) {
	labels, err := s.All()
	if err != nil {
		return nil, err
	}
	out := make(map[string]Label, len(labels))
	for _, label := range labels {
		out[label.FindingID] = label
	}
	return out, nil
}

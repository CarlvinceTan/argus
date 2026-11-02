package swarm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Ledger is every issue recorded for one app across runs, so that a run can
// tell a problem it introduced from one that was already there. One row per
// problem carries its history and reopens when a resolved problem returns
// (after TesterArmy's Issues tab).
//
// Comparing with a ledger never changes it. Only Record does, and only runs
// that should define "known" (main, nightly) should record: a ledger that
// learned from every run would make a regression known on the next rerun.
type Ledger struct {
	Issues []Known `json:"issues"`
}

// Known is one problem's history.
type Known struct {
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	Check     string   `json:"check"`
	Element   string   `json:"element,omitempty"`
	Severity  string   `json:"severity"`
	Status    string   `json:"status"` // open or resolved
	FirstSeen string   `json:"first_seen"`
	LastSeen  string   `json:"last_seen"`
	Runs      int      `json:"runs"`
	Journeys  []string `json:"journeys"`
	Resolved  string   `json:"resolved,omitempty"`
	Reopened  int      `json:"reopened,omitempty"`
}

// Standing values for an issue compared with a ledger.
const (
	StandingNew      = "new"
	StandingKnown    = "known"
	StandingReopened = "reopened"
)

var digits = regexp.MustCompile(`[0-9]+`)

// IssueKey names a problem across runs: its check and element, with numbers read
// as # (as baselines do), so "Cancel Scheduled check 3" and "... 4" are one row.
func IssueKey(issue Issue) string {
	subject := issue.Element
	if subject == "" {
		subject = issue.Title
	}
	return issue.Check + " " + digits.ReplaceAllString(subject, "#")
}

// LoadLedger reads a ledger; a missing file is an empty ledger.
func LoadLedger(path string) (*Ledger, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Ledger{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ledger Ledger
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return nil, fmt.Errorf("reading issue ledger %s: %w", path, err)
	}
	return &ledger, nil
}

// Save writes the ledger atomically, one issue per line block in key order, so
// that it reads as a plain diff.
func (l *Ledger) Save(path string) error {
	sort.Slice(l.Issues, func(i, j int) bool { return l.Issues[i].Key < l.Issues[j].Key })
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func (l *Ledger) find(key string) *Known {
	for index := range l.Issues {
		if l.Issues[index].Key == key {
			return &l.Issues[index]
		}
	}
	return nil
}

// Compare marks each of the report's issues new, known or reopened, and lists
// the open issues this run shows are gone. It doesn't change the ledger.
func (l *Ledger) Compare(r *Report) {
	reported := map[string]bool{}
	for index := range r.Issues {
		issue := &r.Issues[index]
		key := IssueKey(*issue)
		reported[key] = true
		known := l.find(key)
		switch {
		case known == nil:
			issue.Standing = StandingNew
		case known.Status == "resolved":
			issue.Standing, issue.Since = StandingReopened, known.FirstSeen
		default:
			issue.Standing, issue.Since, issue.SeenRuns = StandingKnown, known.FirstSeen, known.Runs
		}
	}
	r.Resolved = nil
	clean := r.cleanJourneys()
	for _, known := range l.Issues {
		if known.Status != "open" || reported[known.Key] {
			continue
		}
		// Gone only where this run looked: an issue whose journeys didn't run, or
		// ran without a screenshot, stays open.
		for _, journey := range known.Journeys {
			if clean[journey] {
				r.Resolved = append(r.Resolved, known.Key)
				break
			}
		}
	}
	sort.Strings(r.Resolved)
}

// cleanJourneys are the journeys this run fully observed.
func (r *Report) cleanJourneys() map[string]bool {
	clean := map[string]bool{}
	for _, result := range r.Results {
		if result.Error == "" && !result.Crashed && result.Blind == 0 {
			clean[result.Journey.ID] = true
		}
	}
	return clean
}

// Record folds a compared report into the ledger.
func (l *Ledger) Record(r Report, now time.Time) {
	day := now.UTC().Format("2006-01-02")
	for _, issue := range r.Issues {
		key := IssueKey(issue)
		known := l.find(key)
		if known == nil {
			l.Issues = append(l.Issues, Known{Key: key, FirstSeen: day})
			known = &l.Issues[len(l.Issues)-1]
		}
		if known.Status == "resolved" {
			known.Reopened++
			known.Resolved = ""
		}
		known.Title, known.Check, known.Element, known.Severity = issue.Title, issue.Check, issue.Element, issue.Severity
		known.Status, known.LastSeen = "open", day
		known.Runs++
		known.Journeys = union(known.Journeys, issue.Journeys)
	}
	for _, key := range r.Resolved {
		if known := l.find(key); known != nil {
			known.Status, known.Resolved = "resolved", day
		}
	}
}

// BlockingNew counts blocking issues the ledger didn't already hold open.
func (r Report) BlockingNew() int {
	count := 0
	for _, issue := range r.Issues {
		if issue.Score >= blockingScore && issue.Standing != StandingKnown {
			count++
		}
	}
	return count
}

func union(a, b []string) []string {
	set := map[string]bool{}
	for _, item := range append(append([]string{}, a...), b...) {
		set[item] = true
	}
	out := make([]string, 0, len(set))
	for item := range set {
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

func (r Report) standingMarkdown(issue Issue) string {
	switch issue.Standing {
	case StandingNew:
		return " — **new**"
	case StandingReopened:
		return fmt.Sprintf(" — **reopened** (first seen %s)", issue.Since)
	case StandingKnown:
		return fmt.Sprintf(" — known since %s, %d runs", issue.Since, issue.SeenRuns)
	}
	return ""
}

func (r Report) resolvedMarkdown(text *strings.Builder) {
	if len(r.Resolved) == 0 {
		return
	}
	text.WriteString("\n## Gone since the ledger\n\n")
	for _, key := range r.Resolved {
		fmt.Fprintf(text, "- %s\n", key)
	}
}

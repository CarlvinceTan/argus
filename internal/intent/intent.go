// Package intent is the ledger of behaviour that looks like a defect but is meant.
//
// It lives in the app's repository, so it is reviewed and versioned with the code
// it describes. Each entry names the source files that make the behaviour true and
// remembers their content. When those files change the entry goes stale and stops
// suppressing anything: an old "this is on purpose" must not hide a new regression.
// Whoever made the change then confirms the intent still holds, or removes it.
package intent

import (
	"crypto/sha256"
	"encoding/hex"
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

// LedgerPath is where the ledger sits in the app's repository.
const LedgerPath = "e2e/intents.json"

// Intent says one finding is expected, and why.
type Intent struct {
	ID string `json:"id"`
	// Check and Element are patterns over a finding's check and element, where *
	// matches anything. Journey narrows it to one journey.
	Check   string `json:"check"`
	Element string `json:"element"`
	Journey string `json:"journey,omitempty"`
	Reason  string `json:"reason"`
	// Paths are the sources that make this true. Digest is their content when it
	// was last confirmed.
	Paths     []string `json:"paths"`
	Digest    string   `json:"digest"`
	Confirmed string   `json:"confirmed"`
}

// Ledger is every recorded intent of one repository.
type Ledger struct {
	Version int      `json:"version"`
	Intents []Intent `json:"intents"`
	repo    string
	stale   map[string]bool
}

// Load reads a repository's ledger; a repository with none gets an empty one.
func Load(repo string) (*Ledger, error) {
	ledger := &Ledger{Version: 1, repo: repo, stale: map[string]bool{}}
	raw, err := os.ReadFile(filepath.Join(repo, LedgerPath))
	if errors.Is(err, fs.ErrNotExist) {
		return ledger, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, ledger); err != nil {
		return nil, fmt.Errorf("reading %s: %w", LedgerPath, err)
	}
	for _, entry := range ledger.Intents {
		digest, err := Digest(repo, entry.Paths)
		ledger.stale[entry.ID] = err != nil || digest != entry.Digest
	}
	return ledger, nil
}

// Digest hashes the named files. A missing file is an error, which makes any
// intent resting on it stale.
func Digest(repo string, paths []string) (string, error) {
	sorted := append([]string{}, paths...)
	sort.Strings(sorted)
	hash := sha256.New()
	for _, name := range sorted {
		content, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "%s\x00%d\x00", name, len(content))
		hash.Write(content)
	}
	return hex.EncodeToString(hash.Sum(nil))[:16], nil
}

// Save writes the ledger in a stable order, so a diff shows only what changed.
func (l *Ledger) Save() error {
	sort.Slice(l.Intents, func(i, j int) bool { return l.Intents[i].ID < l.Intents[j].ID })
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(l.repo, LedgerPath), append(raw, '\n'), 0o644)
}

// Add records a new intent as of the current source. It refuses an entry that
// could not go stale, because that would be a permanent blind spot.
func (l *Ledger) Add(entry Intent) error {
	if entry.ID == "" || entry.Check == "" || entry.Element == "" || entry.Reason == "" {
		return errors.New("an intent needs an id, check, element and reason")
	}
	if len(entry.Paths) == 0 {
		return errors.New("an intent needs the source paths that make it true, or it could never go stale")
	}
	for _, existing := range l.Intents {
		if existing.ID == entry.ID {
			return fmt.Errorf("intent %q already exists; confirm or remove it", entry.ID)
		}
	}
	digest, err := Digest(l.repo, entry.Paths)
	if err != nil {
		return err
	}
	entry.Digest, entry.Confirmed = digest, today()
	l.Intents = append(l.Intents, entry)
	l.stale[entry.ID] = false
	return nil
}

// Confirm says an intent still holds for the current source.
func (l *Ledger) Confirm(id string) error {
	for index := range l.Intents {
		if l.Intents[index].ID != id {
			continue
		}
		digest, err := Digest(l.repo, l.Intents[index].Paths)
		if err != nil {
			return fmt.Errorf("%s rests on a missing file; update or remove it: %w", id, err)
		}
		l.Intents[index].Digest, l.Intents[index].Confirmed = digest, today()
		l.stale[id] = false
		return nil
	}
	return fmt.Errorf("no intent %q", id)
}

// Remove drops an intent that no longer applies.
func (l *Ledger) Remove(id string) error {
	for index := range l.Intents {
		if l.Intents[index].ID == id {
			l.Intents = append(l.Intents[:index], l.Intents[index+1:]...)
			delete(l.stale, id)
			return nil
		}
	}
	return fmt.Errorf("no intent %q", id)
}

// Stale reports whether an intent's sources changed since it was confirmed.
func (l *Ledger) Stale(id string) bool { return l.stale[id] }

func today() string { return time.Now().Format("2006-01-02") }

// Match finds the intent covering a finding.
func (l *Ledger) Match(check, element, journey string) (Intent, bool) {
	if l == nil {
		return Intent{}, false
	}
	for _, entry := range l.Intents {
		if pattern(check, entry.Check) && (entry.Journey == "" || entry.Journey == journey) && pattern(element, entry.Element) {
			return entry, true
		}
	}
	return Intent{}, false
}

func pattern(text, glob string) bool {
	parts := strings.Split(glob, "*")
	for index := range parts {
		parts[index] = regexp.QuoteMeta(parts[index])
	}
	matched, err := regexp.MatchString("^"+strings.Join(parts, ".*")+"$", text)
	return err == nil && matched
}

// Policy states the confirmed intents for a model, so that a reviewer weighing a
// screen knows what is on purpose. Stale ones are left out: they are in question.
func (l *Ledger) Policy() string {
	if l == nil {
		return ""
	}
	lines := []string{}
	for _, entry := range l.Intents {
		if !l.stale[entry.ID] {
			lines = append(lines, fmt.Sprintf("- %s: %s", entry.Element, entry.Reason))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "\nKNOWN INTENTIONAL BEHAVIOUR (not defects)\n" + strings.Join(lines, "\n")
}

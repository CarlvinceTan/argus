package triage

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Calibration is the data the pipeline loads to adjust its own judgement. It is
// deliberately small and inspectable: per-check precision, a suppression and a
// promotion list, and a probability remap built from labelled evidence.
type Calibration struct {
	Schema      int                  `json:"schema"`
	GeneratedAt time.Time            `json:"generated_at"`
	Labels      int                  `json:"labels"`
	Checks      map[string]CheckStat `json:"checks"`
	Suppressed  []string             `json:"suppressed"`
	Promoted    []string             `json:"promoted"`
	Probability []ProbBin            `json:"probability"`

	// ByKind keeps a separate view per subject kind, because what is noise in a
	// terminal is often real in a GUI and the other way round. The top-level
	// fields above are the fallback for a kind with too little history.
	ByKind map[string]KindCalibration `json:"by_kind,omitempty"`

	// Defaults are the fallbacks used for anything unlabelled.
	Unlabelled float64 `json:"unlabelled_weight"`
}

// KindCalibration is the learning for one subject kind (tui, gui).
type KindCalibration struct {
	Labels      int                  `json:"labels"`
	Checks      map[string]CheckStat `json:"checks"`
	Suppressed  []string             `json:"suppressed"`
	Promoted    []string             `json:"promoted"`
	Probability []ProbBin            `json:"probability"`
	Precision   float64              `json:"precision"`
}

// CheckStat is the precision of one check (or check+class pair) over all labels.
type CheckStat struct {
	Samples   int       `json:"samples"`
	Accepted  int       `json:"accepted"`
	Rejected  int       `json:"rejected"`
	Precision float64   `json:"precision"`
	LastLabel time.Time `json:"last_label"`
}

// ProbBin maps a raw model probability band to the precision actually observed,
// which is what turns a chat model's confidence into something usable.
type ProbBin struct {
	Low       float64 `json:"low"`
	High      float64 `json:"high"`
	Samples   int     `json:"samples"`
	Precision float64 `json:"precision"`
}

// Options tune how labels become calibration.
type Options struct {
	MinSamples      int
	SuppressBelow   float64
	PromoteAbove    float64
	MinBinSamples   int
	SuppressWeight  float64
	PromoteWeight   float64
	MaxCheckEntries int
}

// DefaultOptions are conservative: a handful of labels cannot suppress a check,
// because a wrong suppression hides real defects, which is the expensive error.
func DefaultOptions() Options {
	return Options{
		MinSamples:      5,
		SuppressBelow:   0.25,
		PromoteAbove:    0.85,
		MinBinSamples:   10,
		SuppressWeight:  0.5,
		PromoteWeight:   0.85,
		MaxCheckEntries: 500,
	}
}

// Empty returns a calibration that changes nothing.
func Empty() Calibration {
	return Calibration{
		Schema:     1,
		Labels:     0,
		Checks:     map[string]CheckStat{},
		Suppressed: []string{},
		Promoted:   []string{},
		ByKind:     map[string]KindCalibration{},
		Unlabelled: 1.0,
	}
}

// statsFor derives per-check statistics, decisions and the probability remap
// from one set of labels. It is used for the whole history and again per subject
// kind, so the two views cannot drift apart.
func statsFor(labels []Label, opts Options) KindCalibration {
	out := KindCalibration{
		Labels:      len(labels),
		Checks:      map[string]CheckStat{},
		Suppressed:  []string{},
		Promoted:    []string{},
		Probability: []ProbBin{},
	}
	if len(labels) == 0 {
		return out
	}

	// Per check, falling back to the class when a finding has no check id.
	type tally struct {
		accepted int
		rejected int
		last     time.Time
	}
	tallies := map[string]*tally{}
	for _, label := range labels {
		key := label.Check
		if key == "" {
			key = "class:" + label.Class
		}
		entry, ok := tallies[key]
		if !ok {
			entry = &tally{}
			tallies[key] = entry
		}
		// "intentional" and "duplicate" mean "not a new real issue for this
		// pattern"; "misclassified" is still a real issue, so it counts as
		// accepted while its class disagreement is recorded separately.
		switch label.Verdict {
		case Accepted, Misclassified:
			entry.accepted++
		case Rejected, Intentional, Duplicate:
			entry.rejected++
		}
		if label.At.After(entry.last) {
			entry.last = label.At
		}
	}
	keys := make([]string, 0, len(tallies))
	for key := range tallies {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > opts.MaxCheckEntries {
		keys = keys[:opts.MaxCheckEntries]
	}
	acceptedTotal, rejectedTotal := 0, 0
	for _, key := range keys {
		entry := tallies[key]
		samples := entry.accepted + entry.rejected
		if samples == 0 {
			continue
		}
		acceptedTotal += entry.accepted
		rejectedTotal += entry.rejected
		precision := (float64(entry.accepted) + 0.5) / (float64(samples) + 1)
		out.Checks[key] = CheckStat{
			Samples:   samples,
			Accepted:  entry.accepted,
			Rejected:  entry.rejected,
			Precision: precision,
			LastLabel: entry.last,
		}
		if samples >= opts.MinSamples && precision < opts.SuppressBelow {
			out.Suppressed = append(out.Suppressed, key)
		}
		if samples >= opts.MinSamples && precision >= opts.PromoteAbove {
			out.Promoted = append(out.Promoted, key)
		}
	}
	if total := acceptedTotal + rejectedTotal; total > 0 {
		out.Precision = (float64(acceptedTotal) + 0.5) / (float64(total) + 1)
	}

	// Probability remap: decile bins of the raw model probability, each carrying
	// the precision observed for that band.
	bins := make([]struct {
		low, high int
		accepted  int
		rejected  int
	}, 10)
	for index := range bins {
		bins[index].low = index * 10
		bins[index].high = (index + 1) * 10
	}
	for _, label := range labels {
		index := int(clamp(label.DefectProb, 0, 0.999) * 10)
		switch label.Verdict {
		case Accepted, Misclassified:
			bins[index].accepted++
		case Rejected, Intentional, Duplicate:
			bins[index].rejected++
		}
	}
	for _, bin := range bins {
		samples := bin.accepted + bin.rejected
		if samples < opts.MinBinSamples {
			continue
		}
		precision := (float64(bin.accepted) + 0.5) / (float64(samples) + 1)
		out.Probability = append(out.Probability, ProbBin{
			Low:       float64(bin.low) / 100,
			High:      float64(bin.high) / 100,
			Samples:   samples,
			Precision: precision,
		})
	}
	return out
}

// Build derives a calibration from labels.
func Build(labels []Label, opts Options) Calibration {
	if opts.MinSamples <= 0 {
		opts = DefaultOptions()
	}
	calibration := Empty()
	calibration.GeneratedAt = time.Now().UTC()
	calibration.Labels = len(labels)
	if len(labels) == 0 {
		return calibration
	}
	overall := statsFor(labels, opts)
	calibration.Checks = overall.Checks
	calibration.Suppressed = overall.Suppressed
	calibration.Promoted = overall.Promoted
	calibration.Probability = overall.Probability
	calibration.Unlabelled = overall.Precision

	byKind := map[string][]Label{}
	for _, label := range labels {
		kind := label.Kind
		if kind == "" {
			kind = "unknown"
		}
		byKind[kind] = append(byKind[kind], label)
	}
	calibration.ByKind = map[string]KindCalibration{}
	for kind, group := range byKind {
		calibration.ByKind[kind] = statsFor(group, opts)
	}
	return calibration
}

// Load reads a calibration file, returning an inert calibration when absent.
func Load(path string) (Calibration, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Empty(), nil
		}
		return Empty(), fmt.Errorf("reading calibration: %w", err)
	}
	var calibration Calibration
	if err := json.Unmarshal(raw, &calibration); err != nil {
		return Empty(), fmt.Errorf("parsing calibration: %w", err)
	}
	if calibration.Checks == nil {
		calibration.Checks = map[string]CheckStat{}
	}
	return calibration, nil
}

// Save writes a calibration file.
func (c Calibration) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating calibration directory: %w", err)
	}
	encoded, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding calibration: %w", err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return fmt.Errorf("writing calibration: %w", err)
	}
	return nil
}

// IsEmpty reports whether the calibration would change nothing.
func (c Calibration) IsEmpty() bool {
	if len(c.Checks) > 0 || len(c.Suppressed) > 0 || len(c.Promoted) > 0 || len(c.Probability) > 0 {
		return false
	}
	for _, scoped := range c.ByKind {
		if scoped.Labels > 0 {
			return false
		}
	}
	return true
}

// Summary renders a short human-readable description for run manifests.
func (c Calibration) Summary() string {
	if c.IsEmpty() {
		return "none"
	}
	kinds := []string{}
	for kind, scoped := range c.ByKind {
		kinds = append(kinds, fmt.Sprintf("%s=%d", kind, scoped.Labels))
	}
	sort.Strings(kinds)
	return fmt.Sprintf("labels=%d checks=%d suppressed=%d promoted=%d bins=%d kinds[%s]",
		c.Labels, len(c.Checks), len(c.Suppressed), len(c.Promoted), len(c.Probability), strings.Join(kinds, " "))
}

// Adjustment records what a single calibration did, for the finding's audit
// trail: the pipeline explains its own numbers rather than just using them.
type Adjustment struct {
	Adjusted   float64
	Suppressed bool
	Promoted   bool
	Reason     string
}

// shrinkage is the prior strength for blending.
const shrinkage = 3.0

// Apply adjusts a raw defect probability using the learned data. The adjustment
// is a blend, never a replacement: labels inform the pipeline, they do not get
// to silently veto a measured candidate.
func (c Calibration) Apply(kind, check, class string, raw float64) Adjustment {
	state := Adjustment{Adjusted: raw}
	if c.IsEmpty() || raw <= 0 {
		return state
	}
	if kind != "" && len(c.ByKind) > 0 {
		scoped, ok := c.ByKind[kind]
		if !ok || scoped.Labels == 0 {
			// History exists, but none for this kind of subject: a terminal
			// verdict must not shape a GUI judgement, or the reverse.
			return state
		}
		return c.applyWith(scoped.Checks, scoped.Suppressed, scoped.Promoted, scoped.Probability, check, class, raw, state)
	}
	return c.applyWith(c.Checks, c.Suppressed, c.Promoted, c.Probability, check, class, raw, state)
}

// applyWith applies one set of statistics (global or per kind).
func (c Calibration) applyWith(
	checks map[string]CheckStat,
	suppressed []string,
	promoted []string,
	probability []ProbBin,
	check, class string,
	raw float64,
	state Adjustment,
) Adjustment {
	blended := raw
	key := check
	if key == "" {
		key = "class:" + class
	}
	adjustedAlready := false
	if stat, ok := checks[key]; ok && stat.Samples > 0 {
		weight := float64(stat.Samples) / (float64(stat.Samples) + shrinkage)
		blended = (1-weight)*raw + weight*stat.Precision
		state.Reason = fmt.Sprintf("check precision %.2f over %d labels (weight %.2f)", stat.Precision, stat.Samples, weight)
		adjustedAlready = true
	}
	if !adjustedAlready {
		for _, bin := range probability {
			if raw >= bin.Low && raw < bin.High {
				weight := float64(bin.Samples) / (float64(bin.Samples) + shrinkage)
				blended = (1-weight)*raw + weight*bin.Precision
				state.Reason = fmt.Sprintf("probability band %.2f-%.2f precision %.2f over %d labels (weight %.2f)", bin.Low, bin.High, bin.Precision, bin.Samples, weight)
				break
			}
		}
	}
	for _, entry := range suppressed {
		if entry != key {
			continue
		}
		state.Suppressed = true
		if !adjustedAlready {
			// Only apply an extra discount when the check statistic itself did
			// not already reflect the history, so the same labels are not
			// counted twice.
			blended *= c.suppressWeight()
		}
		break
	}
	for _, entry := range promoted {
		if entry == key {
			blended = math.Min(1, blended*1.05+0.05)
			state.Promoted = true
			break
		}
	}
	state.Adjusted = clamp(blended, 0, 1)
	return state
}

func (c Calibration) suppressWeight() float64 {
	if c.Unlabelled > 0 && c.Unlabelled < 1 {
		return math.Max(0.3, c.Unlabelled)
	}
	return 0.5
}

func clamp(value, low, high float64) float64 {
	switch {
	case value < low:
		return low
	case value > high:
		return high
	default:
		return value
	}
}

// Report is a reviewer-facing summary of the label history.
type Report struct {
	Labels      int                  `json:"labels"`
	ByVerdict   map[string]int       `json:"by_verdict"`
	ByCheck     map[string]CheckStat `json:"by_check"`
	NoteCount   int                  `json:"notes"`
	LastLabel   time.Time            `json:"last_label"`
	QuietChecks []string             `json:"quiet_checks"`
}

// Summarise describes the label history.
func Summarise(labels []Label) Report {
	report := Report{
		ByVerdict: map[string]int{},
		ByCheck:   map[string]CheckStat{},
	}
	accepted := map[string]int{}
	rejected := map[string]int{}
	for _, label := range labels {
		report.ByVerdict[string(label.Verdict)]++
		if label.Note != "" {
			report.NoteCount++
		}
		if label.At.After(report.LastLabel) {
			report.LastLabel = label.At
		}
		key := label.Check
		if key == "" {
			key = "class:" + label.Class
		}
		switch label.Verdict {
		case Accepted, Misclassified:
			accepted[key]++
		case Rejected, Intentional, Duplicate:
			rejected[key]++
		}
	}
	report.Labels = len(labels)
	keys := map[string]bool{}
	for key := range accepted {
		keys[key] = true
	}
	for key := range rejected {
		keys[key] = true
	}
	for key := range keys {
		samples := accepted[key] + rejected[key]
		stat := CheckStat{Samples: samples, Accepted: accepted[key], Rejected: rejected[key]}
		if samples > 0 {
			stat.Precision = (float64(stat.Accepted) + 0.5) / (float64(samples) + 1)
		}
		report.ByCheck[key] = stat
		if stat.Samples >= DefaultOptions().MinSamples && stat.Precision < DefaultOptions().SuppressBelow {
			report.QuietChecks = append(report.QuietChecks, key)
		}
	}
	sort.Strings(report.QuietChecks)
	return report
}

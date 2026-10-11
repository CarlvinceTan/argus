// Package agent is the reasoning layer.
//
// The auditor runs the decision tier over a structured snapshot: deterministic
// candidates plus the screen itself plus the collated policy, then turns
// calibrated answers into findings using code-owned thresholds. The explorer
// chooses what to do next with the same decision tier, using a dynamic indexed
// action space (the pattern from browser-use/jev-ultrafast): one operation head
// plus one target head per available operation, validated in code before
// anything is sent to the subject.
package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/carlvincetan/argus/internal/config"
	"github.com/carlvincetan/argus/internal/jev"
	"github.com/carlvincetan/argus/internal/knowledge"
	"github.com/carlvincetan/argus/internal/provider"
	"github.com/carlvincetan/argus/internal/run"
	"github.com/carlvincetan/argus/internal/screen"
	"github.com/carlvincetan/argus/internal/sensors"
	"github.com/carlvincetan/argus/internal/triage"
)

const maxCandidatesPerAudit = 5

// AuditInput is everything known about one probed state.
type AuditInput struct {
	Grid     *screen.Grid
	Motion   sensors.MotionReport
	Checks   []sensors.Evidence
	Action   string
	History  []string
	Subject  run.Subject
	Features []string
}

// AuditResult reports what the audit produced.
type AuditResult struct {
	CandidateTraces []run.CandidateTrace
	Findings        []run.Finding
	Rejected        []run.Finding
	Candidates      int
	DecisionCalls   int
	InputTokens     int
	CostUSD         float64
	Latency         time.Duration
	RawAnswers      map[string]float64
	Error           error
}

// Auditor turns observations into deduplicated findings.
type Auditor struct {
	cfg         config.Config
	providers   provider.Set
	calibration triage.Calibration
	kind        string
	persona     knowledge.Persona
	mu          sync.Mutex

	findings      map[string]*run.Finding
	rejected      []run.Finding
	order         []string
	candidates    int
	calibrated    int
	decisionCalls int
	visionCalls   int
	inputTokens   int
	costUSD       float64
}

// NewAuditor creates an auditor for a session, loading whatever calibration the
// triage history has produced so far.
func NewAuditor(cfg config.Config, providers provider.Set) *Auditor {
	calibration, err := triage.Load(cfg.CalibrationPath)
	if err != nil {
		calibration = triage.Empty()
	}
	return &Auditor{
		cfg:         cfg,
		providers:   providers,
		calibration: calibration,
		kind:        run.KindTUI,
		findings:    map[string]*run.Finding{},
		rejected:    []run.Finding{},
	}
}

// Calibration reports the calibration this auditor is using.
func (a *Auditor) Calibration() triage.Calibration { return a.calibration }

// SetKind tells the auditor which kind of subject it is judging, so the
// calibration history used is the one for that kind.
func (a *Auditor) SetKind(kind string) { a.kind = kind }

// SetPersona narrows the auditor to one specialist reviewer. Personas run over
// the same evidence with different question sets, which is how the run gets
// several independent opinions instead of one shallow one.
func (a *Auditor) SetPersona(persona knowledge.Persona) { a.persona = persona }

// Persona reports the active persona.
func (a *Auditor) Persona() knowledge.Persona { return a.persona }

// Findings returns the current deduplicated findings in insertion order.
func (a *Auditor) Findings() []run.Finding {
	out := make([]run.Finding, 0, len(a.order))
	for _, id := range a.order {
		out = append(out, *a.findings[id])
	}
	run.SortFindings(out)
	return out
}

// Rejected returns findings the triage step declined to report.
func (a *Auditor) Rejected() []run.Finding { return a.rejected }

// Stats returns decision-tier usage.
func (a *Auditor) Stats() (decisionCalls, visionCalls, inputTokens int, costUSD float64) {
	return a.decisionCalls, a.visionCalls, a.inputTokens, a.costUSD
}

// Audit asks the decision tier about one state and folds the answers into the
// finding set.
func (a *Auditor) Audit(ctx context.Context, in AuditInput) AuditResult {
	result := AuditResult{RawAnswers: map[string]float64{}}
	if in.Grid == nil {
		result.Error = fmt.Errorf("audit: no grid")
		return result
	}
	screenName := screen.ScreenName(in.Grid)
	features := in.Features
	if len(features) == 0 {
		features = screen.FeatureLabels(in.Grid)
	}
	rows := strings.Split(in.Grid.TrimmedText(), "\n")
	// Structural measurements belong with the screen: a reviewer has to see what
	// is nested inside what before judging whether the nesting helps.
	checks := append(append([]sensors.Evidence{}, in.Checks...), sensors.RunStructuralChecks(in.Grid)...)
	if a.persona.ID != "" {
		filtered := make([]sensors.Evidence, 0, len(checks))
		for _, evidence := range checks {
			if a.persona.WantsCheck(evidence.Check) {
				filtered = append(filtered, evidence)
			}
		}
		checks = filtered
	}
	candidates := topCandidates(checks, maxCandidatesPerAudit)
	result.Candidates = len(candidates)
	a.candidates += len(candidates)

	for _, candidate := range candidates {
		result.CandidateTraces = append(result.CandidateTraces, run.CandidateTrace{
			Check:      candidate.Check,
			Region:     candidate.Region,
			Detail:     candidate.Detail,
			Snippet:    candidate.Snippet,
			Severity:   candidate.Severity,
			Confidence: candidate.Confidence,
			Verdict:    "candidate",
		})
	}
	state, questions := a.buildRequest(in, screenName, features, rows, candidates)

	started := time.Now()
	repeats := a.cfg.Thresholds.TemporalRepeats
	if repeats < 1 {
		repeats = 1
	}
	decision, err := a.providers.Decisions.Decide(ctx, state, questions, repeats, map[string]any{
		"purpose": "audit",
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
	result.InputTokens = decision.Usage.InputTokens
	result.CostUSD = decision.Usage.CostUSD
	result.DecisionCalls = 1
	for id, answer := range decision.Answers {
		result.RawAnswers[id] = answer.P()
	}

	findings := a.triage(ctx, in, screenName, features, rows, candidates, decision, &result)
	result.Findings = findings
	return result
}

func (a *Auditor) buildRequest(
	in AuditInput,
	screenName string,
	features []string,
	rows []string,
	candidates []sensors.Evidence,
) (map[string]any, map[string]jev.Question) {
	motionDetail := []string{}
	for _, item := range in.Motion.Evidence {
		motionDetail = append(motionDetail, fmt.Sprintf("%s at %s: %s", item.Check, item.Region, item.Detail))
	}
	candidateSummaries := make([]map[string]any, 0, len(candidates))
	for index, candidate := range candidates {
		candidateSummaries = append(candidateSummaries, map[string]any{
			"id": candidateID(index), "check": candidate.Check,
			"region": candidate.Region.String(), "detail": candidate.Detail,
			"snippet": candidate.Snippet, "severity_hint": candidate.Severity,
			"confidence": candidate.Confidence, "measured": candidate.Measured,
		})
	}
	state := map[string]any{
		"app": map[string]any{
			"name":     in.Subject.Name,
			"screen":   screenName,
			"features": features,
			"terminal": fmt.Sprintf("%dx%d", in.Grid.Cols, in.Grid.RowsN),
		},
		"action":  in.Action,
		"history": in.History,
		"screen": map[string]any{
			"text":      numbered(rows, 60),
			"structure": in.Grid.SummaryForModel(),
		},
		"motion": map[string]any{
			"metrics": map[string]any{
				"grids":             in.Motion.Grids,
				"frames":            in.Motion.Frames,
				"idle_change_rate":  in.Motion.IdleChangeRatePerSec,
				"last_input_age_ms": in.Motion.LastInputAge.Milliseconds(),
			},
			"detail": motionDetail,
		},
		"candidates": candidateSummaries,
		"policy":     knowledge.Policy,
	}

	questions := map[string]jev.Question{}
	screenQ := knowledge.ScreenQuestions()
	rowsCriteria := knowledge.RowsCriteria(rows, 24)
	for id, instruction := range screenQ {
		if a.persona.ID != "" && !a.persona.WantsQuestion(id) {
			continue
		}
		switch id {
		case "defect_class", "unstated_what":
			questions[id] = jev.Choice(instruction, knowledge.ClassCriteria())
		case "severity", "unstated_severity":
			questions[id] = jev.Score(instruction, knowledge.SeverityScoreLevels())
		case "defect_row":
			questions[id] = jev.Choice(instruction, rowsCriteria)
		default:
			questions[id] = jev.Noul(instruction)
		}
	}
	for index := range candidates {
		prefix := candidateID(index)
		questions[prefix+"_real_defect"] = jev.Noul(fmt.Sprintf(
			"Is %s a genuine user-visible defect, rather than an intended design choice? Use the measured evidence in state.candidates[%d].", prefix, index))
		questions[prefix+"_class"] = jev.Choice(fmt.Sprintf(
			"Which defect class best fits %s, using state.candidates[%d]?", prefix, index), knowledge.ClassCriteria())
		questions[prefix+"_severity"] = jev.Score(fmt.Sprintf(
			"How much does %s affect a user if real? Use state.candidates[%d].", prefix, index), knowledge.SeverityScoreLevels())
	}
	return state, questions
}

func (a *Auditor) triage(
	ctx context.Context,
	in AuditInput,
	screenName string,
	features []string,
	rows []string,
	candidates []sensors.Evidence,
	decision jev.Result,
	result *AuditResult,
) []run.Finding {
	added := []run.Finding{}
	// The page is the grouping key; a feature label narrows it down further.
	group := screenName
	feature := ""
	if len(features) > 0 {
		feature = features[0]
	}

	pick := func(id string) (jev.Answer, bool) {
		answer, ok := decision.Answers[id]
		return answer, ok
	}
	prob := func(id string) float64 {
		answer, ok := pick(id)
		if !ok {
			return 0
		}
		return answer.P()
	}

	// Candidate findings first: deterministic evidence with a model judgement.
	for index, candidate := range candidates {
		prefix := candidateID(index)
		real, ok := pick(prefix + "_real_defect")
		if !ok {
			continue
		}
		class := knowledge.ClassFromCheck(candidate.Check)
		if answer, ok := pick(prefix + "_class"); ok && answer.Choice != "" && answer.Choice != "none" {
			class = answer.Choice
		}
		severityScore := candidate.Severity
		if answer, ok := pick(prefix + "_severity"); ok {
			severityScore = answer.Score
		}
		userVisible := prob(prefix + "_user_visible")
		finding := run.Finding{
			Title:         knowledge.HumanCheckName(candidate.Check),
			Brief:         candidate.Detail,
			Group:         group,
			Screen:        screenName,
			Feature:       feature,
			Features:      features,
			Region:        clampRegion(candidate.Region, in.Grid),
			Check:         candidate.Check,
			Class:         class,
			Severity:      run.FromScore(severityScore),
			SeverityScore: severityScore,
			DefectProb:    real.P(),
			Confidence:    combineConfidence(real.P(), candidate.Confidence),
			Detail:        candidate.Detail,
			Measured:      candidate.Measured,
			Events:        1,
			FirstSeen:     in.Grid.Time,
			LastSeen:      in.Grid.Time,
			GridID:        in.Grid.ID,
			Verified:      "jev",
			ModelAnswers:  map[string]float64{prefix + "_defect": real.P()},
		}
		if adjusted, ok := measuredCheckProbability(candidate.Check, candidate.Confidence, real.P()); ok {
			finding.DefectProb = adjusted
		} else {
			result.Rejected = append(result.Rejected, finding)
			markCandidate(result, candidate.Check, candidate.Region, "model called it a non-issue")
			continue
		}
		if userVisible > 0 {
			finding.ModelAnswers[prefix+"_user_visible"] = userVisible
		}
		finding.Tags = append(finding.Tags, candidate.Check)
		a.calibrate(&finding, candidate.Check, class)
		if real.P() < a.cfg.Thresholds.DefectProb {
			result.Rejected = append(result.Rejected, finding)
			markCandidate(result, candidate.Check, candidate.Region, fmt.Sprintf("model defect probability %.2f", real.P()))
			continue
		}
		if a.verifyIfUncertain(ctx, &finding, in.Grid, &result.Rejected) {
			continue
		}
		added = append(added, a.merge(finding))
	}

	// Screen-level findings: what a reviewer looking at the screen would flag.
	visible := prob("visible_defect")
	intended := prob("intended")
	severityScore := 0.0
	if answer, ok := pick("severity"); ok {
		severityScore = answer.Score
	}
	class := "other"
	if answer, ok := pick("defect_class"); ok && answer.Choice != "" {
		class = answer.Choice
	}
	rowsCriteria := knowledge.RowsCriteria(rows, 24)
	region := screen.Region{X0: 0, Y0: 0, X1: in.Grid.Cols - 1, Y1: in.Grid.RowsN - 1}
	if answer, ok := pick("defect_row"); ok && answer.Choice != "none" {
		if rowNumber, err := parseRow(answer.Choice, len(rows), rowsCriteria); err == nil {
			region = screen.Region{X0: 0, Y0: rowNumber - 1, X1: in.Grid.Cols - 1, Y1: rowNumber - 1}
		}
	}
	if visible >= a.cfg.Thresholds.DefectProb {
		finding := run.Finding{
			Title:         "screen-level defect: " + strings.ReplaceAll(class, "_", " "),
			Brief:         "A reviewer reading this screen as a whole flagged a defect that the deterministic checks did not localise.",
			Group:         group,
			Screen:        screenName,
			Feature:       feature,
			Features:      features,
			Region:        region,
			Check:         "screen",
			Class:         class,
			Severity:      run.FromScore(severityScore),
			SeverityScore: severityScore,
			DefectProb:    visible,
			Confidence:    visible,
			Detail:        fmt.Sprintf("model defect probability %.2f, severity score %.2f, intended probability %.2f", visible, severityScore, intended),
			Events:        1,
			FirstSeen:     in.Grid.Time,
			LastSeen:      in.Grid.Time,
			GridID:        in.Grid.ID,
			Verified:      "jev",
			ModelAnswers:  map[string]float64{"visible_defect": visible, "intended": intended},
		}
		a.calibrate(&finding, "screen", class)
		if intended >= 0.7 {
			finding.Intentional = true
			result.Rejected = append(result.Rejected, finding)
		} else {
			if a.verifyIfUncertain(ctx, &finding, in.Grid, &result.Rejected) {
				// conflicting verification: keep in rejected, not in the backlog
			} else {
				added = append(added, a.merge(finding))
			}
		}
	}

	// Unstated-expectation findings: the "obvious even though nobody said it"
	// class the user asked for. These come from POLICY, not from a check.
	if unstated := prob("unstated"); unstated >= a.cfg.Thresholds.DefectProb {
		class := "guidance_missing"
		if answer, ok := pick("unstated_what"); ok {
			switch answer.Choice {
			case "", "none":
				class = "guidance_missing"
			default:
				class = answer.Choice
			}
		}
		severityScore := 2.0
		if answer, ok := pick("unstated_severity"); ok {
			severityScore = answer.Score
		}
		finding := run.Finding{
			Title:         "unstated expectation: " + strings.ReplaceAll(class, "_", " "),
			Brief:         "This screen breaks an expectation that is not written down anywhere but that users of similar UIs treat as obvious.",
			Group:         group,
			Screen:        screenName,
			Feature:       feature,
			Features:      features,
			Region:        region,
			Check:         "policy",
			Class:         class,
			Severity:      run.FromScore(severityScore),
			SeverityScore: severityScore,
			DefectProb:    unstated,
			Confidence:    unstated,
			Detail:        "raised by the POLICY expectations block, not by a deterministic check",
			Events:        1,
			FirstSeen:     in.Grid.Time,
			LastSeen:      in.Grid.Time,
			GridID:        in.Grid.ID,
			Verified:      "jev",
			ModelAnswers:  map[string]float64{"unstated": unstated},
			Tags:          []string{"unstated_expectation"},
		}
		a.calibrate(&finding, "policy", class)
		if a.verifyIfUncertain(ctx, &finding, in.Grid, &result.Rejected) {
			// rejected by blinded verification
		} else {
			added = append(added, a.merge(finding))
		}
	}

	// Individual policy questions that are defect-positive on their own.
	policyQuestions := []struct {
		id    string
		title string
		class string
	}{
		{"guidance", "blocked or empty state with no way forward", "guidance_missing"},
		{"focus_missing", "interactive area with no visible focus", "focus_missing"},
		{"busy_feedback", "claims work in progress without feedback", "guidance_missing"},
		{"inconsistent", "inconsistent formatting or affordances", "inconsistency"},
	}
	for _, entry := range policyQuestions {
		value := prob(entry.id)
		// These questions are speculative by nature (nothing was measured), so
		// they must clear a higher bar than a deterministic candidate.
		if value < a.cfg.Thresholds.PolicyProb {
			continue
		}
		severityScore := 2.0
		if answer, ok := pick("severity"); ok && answer.Score > severityScore {
			severityScore = answer.Score
		}
		finding := run.Finding{
			Title:         entry.title,
			Brief:         "Raised by the POLICY expectations block: " + entry.id,
			Group:         group,
			Screen:        screenName,
			Feature:       feature,
			Features:      features,
			Region:        region,
			Check:         "policy:" + entry.id,
			Class:         entry.class,
			Severity:      run.FromScore(severityScore),
			SeverityScore: severityScore,
			DefectProb:    value,
			Confidence:    value,
			Events:        1,
			FirstSeen:     in.Grid.Time,
			LastSeen:      in.Grid.Time,
			GridID:        in.Grid.ID,
			Verified:      "jev",
			ModelAnswers:  map[string]float64{entry.id: value},
			Tags:          []string{"policy"},
		}
		a.calibrate(&finding, finding.Check, entry.class)
		if a.verifyIfUncertain(ctx, &finding, in.Grid, &result.Rejected) {
			continue
		}
		added = append(added, a.merge(finding))
	}

	// Motion findings are audited separately so an animation defect is never
	// confused with a static one.
	if len(in.Motion.Evidence) > 0 {
		added = append(added, a.auditMotion(ctx, in, screenName, group, features, result)...)
	}
	return added
}

func (a *Auditor) auditMotion(
	ctx context.Context,
	in AuditInput,
	screenName string,
	group string,
	features []string,
	result *AuditResult,
) []run.Finding {
	questions := map[string]jev.Question{
		"real_motion_defect": jev.Noul(knowledge.MotionQuestion()),
		"motion_severity":    jev.Score("How much would a user be affected by `motion.detail`?", knowledge.SeverityScoreLevels()),
	}
	state := map[string]any{
		"app":    map[string]any{"name": in.Subject.Name, "screen": screenName},
		"action": in.Action,
		"motion": map[string]any{"detail": motionDetailStrings(in.Motion), "metrics": in.Motion.IdleChangeRatePerSec},
		"screen": map[string]any{"text": numbered(strings.Split(in.Grid.TrimmedText(), "\n"), 40)},
		"policy": knowledge.Policy,
	}
	decision, err := a.providers.Decisions.Decide(ctx, state, questions, 1, map[string]any{"purpose": "motion"})
	if err != nil {
		result.Rejected = append(result.Rejected, run.Finding{Title: "motion audit failed", Detail: err.Error()})
		return nil
	}
	a.decisionCalls++
	a.inputTokens += decision.Usage.InputTokens
	a.costUSD += decision.Usage.CostUSD
	real := decision.Answers["real_motion_defect"].P()
	severityScore := 2.0
	if answer, ok := decision.Answers["motion_severity"]; ok {
		severityScore = answer.Score
	}
	if real < a.cfg.Thresholds.DefectProb {
		return nil
	}
	out := []run.Finding{}
	feature := ""
	if len(features) > 0 {
		feature = features[0]
	}
	for _, evidence := range in.Motion.Evidence {
		finding := run.Finding{
			Title:         knowledge.HumanCheckName(evidence.Check),
			Brief:         evidence.Detail,
			Group:         group,
			Screen:        screenName,
			Feature:       feature,
			Features:      features,
			Region:        clampRegion(evidence.Region, in.Grid),
			Check:         evidence.Check,
			Class:         knowledge.ClassFromCheck(evidence.Check),
			Severity:      run.FromScore(severityScore),
			SeverityScore: severityScore,
			DefectProb:    real * evidence.Confidence,
			Confidence:    combineConfidence(real, evidence.Confidence),
			Detail:        evidence.Detail,
			Measured:      evidence.Measured,
			Events:        1,
			FirstSeen:     in.Grid.Time,
			LastSeen:      in.Grid.Time,
			GridID:        in.Grid.ID,
			Verified:      "jev",
			ModelAnswers:  map[string]float64{"real_motion_defect": real},
			Tags:          []string{"motion", evidence.Check},
		}
		a.calibrate(&finding, evidence.Check, finding.Class)
		if finding.DefectProb < a.cfg.Thresholds.DefectProb {
			result.Rejected = append(result.Rejected, finding)
			continue
		}
		out = append(out, a.merge(finding))
	}
	return out
}

// calibrate folds the triage history into a finding's probability and records
// what changed, so a reviewer can see why a number moved.
func (a *Auditor) calibrate(finding *run.Finding, check, class string) {
	state := a.calibration.Apply(a.kind, check, class, finding.DefectProb)
	if state.Adjusted == finding.DefectProb {
		return
	}
	finding.Detail = strings.TrimSpace(fmt.Sprintf("%s | calibrated from %.2f to %.2f (%s)", finding.Detail, finding.DefectProb, state.Adjusted, state.Reason))
	finding.Tags = append(finding.Tags, "calibrated")
	if state.Suppressed {
		finding.Tags = append(finding.Tags, "suppressed_by_history")
	}
	if state.Promoted {
		finding.Tags = append(finding.Tags, "promoted_by_history")
	}
	finding.DefectProb = state.Adjusted
	finding.Confidence = minFloat(1, finding.Confidence)
	a.calibrated++
}

// verifyIfUncertain asks the vision tier for a blinded description when the
// decision tier is unsure. Returns true when the verification contradicts the
// finding, in which case the caller should not report it.
func (a *Auditor) verifyIfUncertain(ctx context.Context, finding *run.Finding, grid *screen.Grid, rejected *[]run.Finding) bool {
	thresholds := a.cfg.Thresholds
	major := finding.Severity >= run.SevMajor
	uncertain := finding.DefectProb >= thresholds.VerifyLow && finding.DefectProb < thresholds.VerifyHigh
	if !uncertain && !(major && finding.DefectProb < 0.9) {
		return false
	}
	if a.providers.Vision == nil || a.visionCalls >= a.cfg.Limits.MaxVisionCalls {
		// No vision tier available: keep the finding but mark it unverified.
		if uncertain {
			finding.Verified = "unverified"
		}
		return false
	}
	crop := cropFor(grid, finding.Region)
	verification, err := a.Verify(ctx, *finding, grid, crop)
	if err != nil {
		finding.Tags = append(finding.Tags, "verification_failed")
		return false
	}
	finding.Verified = "vision"
	if !verification.Confirmed {
		finding.Intentional = true
		finding.Detail = strings.TrimSpace(finding.Detail + " | blinded vision description did not mention this defect class")
		*rejected = append(*rejected, *finding)
		return true
	}
	finding.Confidence = minFloat(1, finding.Confidence+0.1)
	finding.Detail = strings.TrimSpace(finding.Detail + " | blinded vision description supported it")
	return false
}

func cropFor(grid *screen.Grid, region screen.Region) *screen.Grid {
	padX, padY := 8, 2
	return grid.Crop(
		region.X0-padX,
		region.Y0-padY,
		region.X1+padX+1,
		region.Y1+padY+1,
	)
}

// overlapping finds an existing finding that describes the same defect in the
// same page with a region that intersects (or sits within two rows of) the new
// one. Without this, a panel that shifts by a row produces a second finding.
func (a *Auditor) overlapping(finding run.Finding, identityClass string) (string, bool) {
	for _, id := range a.order {
		existing, ok := a.findings[id]
		if !ok {
			continue
		}
		if existing.Check != finding.Check || existing.Group != finding.Group {
			continue
		}
		existingClass := existing.Class
		if existing.Check == "screen" {
			existingClass = "screen"
		}
		if strings.HasPrefix(existing.Check, "policy") {
			existingClass = existing.Check
		}
		if existingClass != identityClass {
			continue
		}
		if regionsOverlap(existing.Region, finding.Region, 2) {
			return id, true
		}
	}
	return "", false
}

// regionsOverlap reports whether two regions touch, allowing a row/column slack.
func regionsOverlap(a, b screen.Region, slack int) bool {
	return a.X0-slack <= b.X1+slack && b.X0-slack <= a.X1+slack &&
		a.Y0-slack <= b.Y1+slack && b.Y0-slack <= a.Y1+slack
}

// reportable decides whether a finding reaches the backlog. Code owns this, not
// the model: severity floor, plus a rule that an uncertain finding whose vision
// verification never ran stays out of the backlog instead of being reported as
// if it were confirmed.
func (a *Auditor) reportable(finding run.Finding) bool {
	if finding.Intentional || finding.SeverityScore < a.cfg.Thresholds.SeverityFloor {
		return false
	}
	// The probability checked here is the *calibrated* one, so review history
	// can demote a pattern the reviewer has repeatedly called noise. This is the
	// only place that decides what reaches the backlog, which keeps the learning
	// effect auditable.
	if finding.DefectProb < a.cfg.Thresholds.DefectProb {
		return false
	}
	if finding.Verified == "unverified" && finding.DefectProb < a.cfg.Thresholds.VerifyHigh {
		return false
	}
	return true
}

// merge deduplicates a finding into the set, keeping the strongest evidence.
func (a *Auditor) merge(finding run.Finding) run.Finding {
	// Screen-wide findings (policy, unstated expectations, whole-screen defects)
	// are identified per page rather than per row: the same missing affordance
	// should not be reported once per row it happens to be spotted on.
	identityRegion := finding.Region
	if strings.HasPrefix(finding.Check, "policy") || finding.Check == "screen" {
		identityRegion = screen.Region{X0: 0, Y0: 0, X1: 0, Y1: 0}
	}
	identityClass := finding.Class
	switch {
	case finding.Check == "screen":
		identityClass = "screen" // one screen-level finding per page, not per class
	case strings.HasPrefix(finding.Check, "policy"):
		identityClass = finding.Check
	}
	// A defect that moves a few rows between repaints is the same defect: if a
	// finding of the same check, class and page already covers an overlapping
	// region, merge into it rather than reporting a near-duplicate.
	if existingID, ok := a.overlapping(finding, identityClass); ok {
		return a.mergeInto(existingID, finding)
	}
	id := run.FindingID(finding.Check, finding.Group, identityClass, identityRegion)
	finding.ID = id
	return a.mergeInto(id, finding)
}

// mergeInto folds one finding into an existing entry.
func (a *Auditor) mergeInto(id string, finding run.Finding) run.Finding {
	existing, ok := a.findings[id]
	if !ok {
		finding.SeverityName = finding.Severity.String()
		finding.Reported = a.reportable(finding)
		a.findings[id] = &finding
		a.order = append(a.order, id)
		return finding
	}
	existing.Events++
	if finding.DefectProb > existing.DefectProb {
		existing.DefectProb = finding.DefectProb
	}
	if finding.Severity > existing.Severity {
		existing.Severity = finding.Severity
		existing.SeverityScore = finding.SeverityScore
		existing.SeverityName = finding.Severity.String()
	}
	if finding.Confidence > existing.Confidence {
		existing.Confidence = finding.Confidence
	}
	if finding.LastSeen > existing.LastSeen {
		existing.LastSeen = finding.LastSeen
		existing.GridID = finding.GridID
	}
	if finding.Intentional {
		existing.Intentional = true
	}
	existing.Reported = a.reportable(*existing)
	return *existing
}

// markCandidate updates the trace for a deterministic candidate with the
// model's verdict, so a reviewer can see why it was dropped.
func markCandidate(result *AuditResult, check string, region screen.Region, verdict string) {
	for index := range result.CandidateTraces {
		trace := &result.CandidateTraces[index]
		if trace.Check == check && trace.Region == region {
			trace.Verdict = verdict
			return
		}
	}
}

// measuredCheckProbability blends a measurement with the model's judgement for
// checks whose evidence is something code measured rather than something a model
// saw: how deeply boxes are nested, whether a feature exists at all. The model
// still decides how much it matters (severity) and can still call it a non-issue,
// but it cannot make a measurement disappear.
func measuredCheckProbability(check string, confidence, modelProbability float64) (float64, bool) {
	if !strings.HasPrefix(check, "structure") {
		return modelProbability, true
	}
	if modelProbability < 0.2 {
		return 0, false
	}
	return clampUnit(0.7*confidence + 0.3*modelProbability), true
}

func candidateID(index int) string {
	return fmt.Sprintf("cand%d", index+1)
}

// topCandidates keeps the candidate set broad rather than deep: several checks
// can each see a different defect on one screen, so a flat "top five by score"
// silently loses whole defect classes. At most two candidates per check are
// kept, highest scoring first.
func topCandidates(evidence []sensors.Evidence, limit int) []sensors.Evidence {
	sorted := append([]sensors.Evidence{}, evidence...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Severity*sorted[i].Confidence > sorted[j].Severity*sorted[j].Confidence
	})
	perCheck := map[string]int{}
	out := make([]sensors.Evidence, 0, limit)
	for _, candidate := range sorted {
		if perCheck[candidate.Check] >= 2 {
			continue
		}
		perCheck[candidate.Check]++
		out = append(out, candidate)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func numbered(rows []string, limit int) string {
	builder := strings.Builder{}
	count := 0
	for index, row := range rows {
		if count >= limit {
			builder.WriteString(fmt.Sprintf("... %d more rows\n", len(rows)-index))
			break
		}
		builder.WriteString(fmt.Sprintf("%3d|%s\n", index+1, row))
		count++
	}
	return builder.String()
}

func combineConfidence(modelProb, deterministic float64) float64 {
	return minFloat(1, 0.6*modelProb+0.4*deterministic)
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func clampRegion(region screen.Region, grid *screen.Grid) screen.Region {
	region.X0 = maxInt(region.X0, 0)
	region.Y0 = maxInt(region.Y0, 0)
	region.X1 = minInt(region.X1, grid.Cols-1)
	region.Y1 = minInt(region.Y1, grid.RowsN-1)
	if region.X1 < region.X0 {
		region.X1 = region.X0
	}
	if region.Y1 < region.Y0 {
		region.Y1 = region.Y0
	}
	return region
}

func parseRow(label string, rowCount int, criteria map[string]string) (int, error) {
	if _, ok := criteria[label]; !ok {
		return 0, fmt.Errorf("row %q not in criteria", label)
	}
	value := 0
	for _, char := range label {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("row %q is not numeric", label)
		}
		value = value*10 + int(char-'0')
	}
	if value < 1 || value > rowCount {
		return 0, fmt.Errorf("row %d out of range", value)
	}
	return value, nil
}

func motionDetailStrings(report sensors.MotionReport) []string {
	out := make([]string, 0, len(report.Evidence))
	for _, item := range report.Evidence {
		out = append(out, fmt.Sprintf("%s at %s: %s", item.Check, item.Region, item.Detail))
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

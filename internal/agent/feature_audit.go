package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/carlvincetan/argus/internal/audit"
	"github.com/carlvincetan/argus/internal/jev"
	"github.com/carlvincetan/argus/internal/knowledge"
	"github.com/carlvincetan/argus/internal/run"
	"github.com/carlvincetan/argus/internal/screen"
)

const (
	maxInventoryCandidates = 5
	featureAuditPolicy     = `AUDITOR STANCE
You are auditing software completeness and behaviour, not pixels.
- You are given a MEASURED INVENTORY: features that were actually observed, screens that were visited, lists that were seen with the actions they advertised, and moves between screens. Treat it as the truth about what exists.
- A capability that appears in the inventory in any form (a screen, an advertised control, a move) EXISTS. Never report it as missing.
- Report what is missing or wrong *given* what exists: a sign-in that offers no way to recover an account, a list whose rows cannot be opened or acted on, a destructive action with no confirmation, a screen that only lets the user leave.
- Judge the application as a user of this kind of software would, not as its author. A gap that forces the user to ask for help is serious; a missing convenience is minor.
- Do not invent features the inventory does not mention, and do not propose implementation work: name the gap, not the fix.`
)

// AuditInventory judges the measured feature inventory: which companions are
// missing, which behaviours are wrong, and how much each matters.
//
// Code decided what exists; this asks the decision tier what the absence means.
// The split matters: a model asked "does this app have password recovery?" will
// answer from imagination, while a model asked "given this inventory, is the
// absence of password recovery a problem here?" is judging a fact.
func (a *Auditor) AuditInventory(
	ctx context.Context,
	inventory *audit.Inventory,
	subject run.Subject,
) AuditResult {
	result := AuditResult{RawAnswers: map[string]float64{}}
	if inventory == nil || len(inventory.Hits) == 0 {
		return result
	}
	candidates := audit.Candidates(inventory)
	if len(candidates) > maxInventoryCandidates {
		candidates = candidates[:maxInventoryCandidates]
	}
	result.Candidates = len(candidates)

	state := map[string]any{
		"app": map[string]any{
			"name":   subject.Name,
			"kind":   subject.Terminal,
			"policy": featureAuditPolicy,
		},
		"inventory": inventory.Summary(),
		"structure": map[string]any{
			"max_nesting":      inventory.Structure.MaxNesting,
			"dead_end_screens": inventory.Structure.DeadEndScreens,
			"inert_controls":   inventory.Structure.InertAffordances,
			"repeated_values":  inventory.Structure.RepeatedValues,
			"screens":          inventory.Screens,
		},
		"candidates": candidateSummaries(candidates),
		"policy":     featureAuditPolicy,
	}

	questions := map[string]jev.Question{}
	for index, candidate := range candidates {
		prefix := candidateID(index)
		detail := fmt.Sprintf("kind=%s, title=%s, detail=%s, severity hint %.1f",
			candidate.Kind, candidate.Title, candidate.Detail, candidate.Severity)
		result.CandidateTraces = append(result.CandidateTraces, run.CandidateTrace{
			Check:      "audit:" + candidate.Kind,
			Region:     candidate.Region,
			Detail:     candidate.Title + " — " + candidate.Detail,
			Severity:   candidate.Severity,
			Confidence: candidate.Confidence,
			Verdict:    "candidate",
		})
		questions[prefix+"_real"] = jev.Noul(fmt.Sprintf(
			"For this application, is the following a genuine problem a user would hit (%s)? Answer yes only if the inventory supports it.", detail))
		questions[prefix+"_class"] = jev.Choice(
			fmt.Sprintf("Which class fits this problem best? %s", detail), knowledge.AuditClassCriteria())
		questions[prefix+"_severity"] = jev.Score(
			fmt.Sprintf("How much does this problem affect a user? %s", detail), knowledge.SeverityScoreLevels())
	}
	// Ask for anything the inventory itself might have missed, constrained to the
	// feature vocabulary so the answer stays grounded.
	questions["inventory_gap"] = jev.Noul(
		"Given the inventory, is there an important capability missing from this application that none of the candidates above describe?")
	questions["inventory_gap_what"] = jev.Choice(
		"If something important is missing, which capability is it?", knowledge.FeatureCriteria())
	questions["inventory_gap_severity"] = jev.Score(
		"How much would that missing capability affect a user?", knowledge.SeverityScoreLevels())

	repeats := a.cfg.Thresholds.TemporalRepeats
	if repeats < 1 {
		repeats = 1
	}
	if a.cfg.Provider == "local" || a.cfg.Provider == "opensource" {
		repeats = 1
	}
	started := time.Now()
	decision, err := a.providers.Decisions.Decide(ctx, state, questions, repeats, map[string]any{
		"purpose": "feature-audit",
		"subject": subject.Name,
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

	screenName := "app:" + subject.Name
	added := []run.Finding{}
	for index, candidate := range candidates {
		prefix := candidateID(index)
		real, ok := decision.Answers[prefix+"_real"]
		if !ok {
			continue
		}
		// A measured absence does not need the model's permission to exist: code
		// established that the feature is not there. The model decides how much
		// it matters (severity), and can still call it a non-issue, but it
		// cannot make a fact disappear. Visual findings keep the opposite rule,
		// because there the model is judging whether a measurement is a defect.
		modelProbability := real.P()
		measured := candidate.Confidence
		blended := 0.7*measured + 0.3*modelProbability
		if modelProbability < 0.2 || blended < a.cfg.Thresholds.DefectProb {
			if modelProbability < 0.2 {
				markCandidate(&result, "audit:"+candidate.Kind, candidate.Region, fmt.Sprintf("model called it a non-issue (%.2f)", modelProbability))
				continue
			}
		}
		class := defaultAuditClass(candidate.Kind)
		if answer, ok := decision.Answers[prefix+"_class"]; ok && answer.Choice != "" && answer.Choice != "none" {
			class = answer.Choice
		}
		severityScore := candidate.Severity
		if answer, ok := decision.Answers[prefix+"_severity"]; ok {
			severityScore = answer.Score
		}
		group := auditGroup(candidate)
		finding := run.Finding{
			Title:         candidate.Title,
			Brief:         candidate.Detail,
			Group:         group,
			Screen:        screenName,
			Feature:       group,
			Region:        candidate.Region,
			Check:         "audit:" + candidate.Kind,
			Class:         class,
			Severity:      run.FromScore(severityScore),
			SeverityScore: severityScore,
			DefectProb:    clampUnit(blended),
			Confidence:    combineConfidence(modelProbability, candidate.Confidence),
			Detail:        candidate.Detail,
			Events:        1,
			FirstSeen:     0,
			LastSeen:      0,
			Verified:      "jev",
			ModelAnswers:  map[string]float64{prefix + "_real": modelProbability},
			Tags:          []string{"audit", candidate.Kind},
		}
		for _, missing := range candidate.Missing {
			finding.Tags = append(finding.Tags, "missing:"+string(missing))
		}
		if a.persona.ID != "" {
			finding.Tags = append(finding.Tags, "persona:"+a.persona.ID)
		}
		a.calibrate(&finding, finding.Check, class)
		added = append(added, a.merge(finding))
	}

	// Anything the reviewer raised itself, still constrained to the vocabulary.
	if answer, ok := decision.Answers["inventory_gap"]; ok && answer.P() >= 0.7 {
		if what, ok := decision.Answers["inventory_gap_what"]; ok && what.Choice != "" && what.Choice != "none" {
			severityScore := 2.0
			if score, ok := decision.Answers["inventory_gap_severity"]; ok {
				severityScore = score.Score
			}
			group := "app:" + what.Choice
			finding := run.Finding{
				Title:         fmt.Sprintf("no %s anywhere in the application", what.Choice),
				Brief:         "The reviewer found no trace of this capability in the measured inventory.",
				Group:         group,
				Screen:        screenName,
				Feature:       group,
				Check:         "audit:feature_gap",
				Class:         "feature_missing",
				Severity:      run.FromScore(severityScore),
				SeverityScore: severityScore,
				DefectProb:    0.7,
				Confidence:    answer.P(),
				Detail:        "raised by the completeness reviewer against the inventory",
				Events:        1,
				Verified:      "jev",
				ModelAnswers:  map[string]float64{"inventory_gap": answer.P()},
				Tags:          []string{"audit", "feature_gap", "missing:" + what.Choice},
			}
			if a.persona.ID != "" {
				finding.Tags = append(finding.Tags, "persona:"+a.persona.ID)
			}
			a.calibrate(&finding, finding.Check, finding.Class)
			added = append(added, a.merge(finding))
		}
	}
	result.Findings = added
	return result
}

func clampUnit(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

func candidateSummaries(candidates []audit.Candidate) []string {
	out := make([]string, 0, len(candidates))
	for index, candidate := range candidates {
		out = append(out, fmt.Sprintf("%s: kind=%s title=%s detail=%s severity_hint=%.1f",
			candidateID(index), candidate.Kind, candidate.Title, candidate.Detail, candidate.Severity))
	}
	return out
}

func defaultAuditClass(kind string) string {
	switch kind {
	case "feature_gap":
		return "feature_missing"
	case "structure":
		return "structure"
	case "behaviour":
		return "behaviour"
	default:
		return "other"
	}
}

// auditGroup buckets findings by the capability they concern, so the backlog can
// be read as "everything about auth", "everything about navigation".
func auditGroup(candidate audit.Candidate) string {
	if len(candidate.Missing) > 0 {
		return "app:" + string(candidate.Missing[0])
	}
	switch candidate.Kind {
	case "structure":
		return "app:structure"
	case "behaviour":
		return "app:behaviour"
	default:
		return "app:general"
	}
}

var _ = screen.Region{}

package jev

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
)

// rawAnswer mirrors the wire format of one answer. Pointers distinguish
// "absent" from "zero" so an omitted field is an error rather than a silent 0.
type rawAnswer struct {
	Noul          *float64           `json:"noul"`
	Choice        string             `json:"choice"`
	Score         *float64           `json:"score"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence"`
}

// criteriaLabels normalises the criteria field (built as either a map or a
// slice, and possibly decoded back from JSON) into an ordered label list.
func criteriaLabels(criteria any) (map[string]bool, []string, error) {
	switch typed := criteria.(type) {
	case map[string]string:
		labels := make([]string, 0, len(typed))
		for label := range typed {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		return toSet(labels), labels, nil
	case []string:
		return toSet(typed), append([]string{}, typed...), nil
	case []any:
		labels := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, nil, errors.New("criteria list must contain strings")
			}
			labels = append(labels, text)
		}
		return toSet(labels), labels, nil
	case map[string]any:
		labels := make([]string, 0, len(typed))
		for label := range typed {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		return toSet(labels), labels, nil
	default:
		return nil, nil, fmt.Errorf("unsupported criteria type %T", criteria)
	}
}

func toSet(labels []string) map[string]bool {
	set := make(map[string]bool, len(labels))
	for _, label := range labels {
		set[label] = true
	}
	return set
}

// Validate checks one answer against its question and returns it typed.
func Validate(id string, question Question, raw json.RawMessage) (Answer, error) {
	var parsed rawAnswer
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Answer{}, fmt.Errorf("question %q: decoding answer: %w", id, err)
	}

	switch question.Type {
	case "noul":
		if parsed.Noul == nil || !finiteProbability(*parsed.Noul) {
			return Answer{}, fmt.Errorf("question %q: invalid noul", id)
		}
		return Answer{ID: id, Type: "noul", Noul: *parsed.Noul}, nil

	case "choice":
		keys, _, err := criteriaLabels(question.Criteria)
		if err != nil {
			return Answer{}, fmt.Errorf("question %q: %w", id, err)
		}
		if !keys[parsed.Choice] {
			return Answer{}, fmt.Errorf("question %q: choice %q outside criteria", id, parsed.Choice)
		}
		if err := validateDistribution(id, parsed.Probabilities, keys); err != nil {
			return Answer{}, err
		}
		confidence, err := optionalConfidence(id, parsed.Confidence)
		if err != nil {
			return Answer{}, err
		}
		// Ties are legitimate: a calibrated model may split 0.5/0.5 and name
		// either side. Only a clearly non-maximal pick is rejected.
		if parsed.Probabilities[parsed.Choice] < maxProbability(parsed.Probabilities)-0.02 {
			return Answer{}, fmt.Errorf("question %q: choice %q is not the argmax", id, parsed.Choice)
		}
		return Answer{
			ID:            id,
			Type:          "choice",
			Choice:        parsed.Choice,
			Probabilities: parsed.Probabilities,
			Confidence:    confidence,
		}, nil

	case "score":
		_, ordered, err := criteriaLabels(question.Criteria)
		if err != nil {
			return Answer{}, fmt.Errorf("question %q: %w", id, err)
		}
		if len(ordered) < 2 {
			return Answer{}, fmt.Errorf("question %q: score needs at least two levels", id)
		}
		keys := make(map[string]bool, len(ordered))
		for index := range ordered {
			keys[strconv.Itoa(index)] = true
		}
		if err := validateDistribution(id, parsed.Probabilities, keys); err != nil {
			return Answer{}, err
		}
		confidence, err := optionalConfidence(id, parsed.Confidence)
		if err != nil {
			return Answer{}, err
		}
		if parsed.Score == nil || *parsed.Score < 0 || *parsed.Score > float64(len(ordered)-1)+1e-6 {
			return Answer{}, fmt.Errorf("question %q: score outside legend range", id)
		}
		return Answer{
			ID:            id,
			Type:          "score",
			Score:         *parsed.Score,
			Probabilities: parsed.Probabilities,
			Confidence:    confidence,
		}, nil

	default:
		return Answer{}, fmt.Errorf("question %q: unknown type %q", id, question.Type)
	}
}

func validateDistribution(id string, probabilities map[string]float64, keys map[string]bool) error {
	if len(probabilities) != len(keys) {
		return fmt.Errorf("question %q: probabilities do not match criteria", id)
	}
	total := 0.0
	for label, value := range probabilities {
		if !keys[label] {
			return fmt.Errorf("question %q: unexpected label %q", id, label)
		}
		if !finiteProbability(value) {
			return fmt.Errorf("question %q: label %q is not a probability", id, label)
		}
		total += value
	}
	if math.Abs(total-1) > 0.05 {
		return fmt.Errorf("question %q: probabilities sum to %.3f", id, total)
	}
	return nil
}

func optionalConfidence(id string, confidence *float64) (float64, error) {
	if confidence == nil {
		return 0, nil
	}
	if !finiteProbability(*confidence) {
		return 0, fmt.Errorf("question %q: invalid confidence", id)
	}
	return *confidence, nil
}

func maxProbability(probabilities map[string]float64) float64 {
	best := 0.0
	for _, value := range probabilities {
		if value > best {
			best = value
		}
	}
	return best
}

func validate(id string, question Question, raw json.RawMessage) (Answer, error) {
	return Validate(id, question, raw)
}

func readAllLimited(reader io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(reader, limit))
}

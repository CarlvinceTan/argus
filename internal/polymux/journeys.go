package polymux

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Journey is one fixture state to explore. Polymux's E2E flows already declare
// the states worth reaching and which sources affect them, so journeys come from
// that map rather than from a model.
type Journey struct {
	ID    string   `json:"id"`
	Flows []string `json:"flows"`
	Flags []string `json:"flags"`
	// Linked says why a journey the area rules left out runs anyway: its
	// approved baseline shows a label from a changed source.
	Linked string `json:"linked,omitempty"`
}

// appSources mirrors scripts/e2e.py: an unmapped change here could affect anything.
var appSources = []string{"apps/Polymux/", "apps/server/", "packages/", "contracts/", "Package.swift", "go.work", "scripts/run"}

type areaMap struct {
	Ignore []string `json:"ignore"`
	Always []string `json:"always"`
	Areas  []struct {
		Paths []string `json:"paths"`
		Flows []string `json:"flows"`
	} `json:"areas"`
}

var fixtureLine = regexp.MustCompile(`^// @fixture (.+)$`)

// fnmatch matches like Python's: unlike path.Match, * crosses a slash.
func fnmatch(name, pattern string) bool {
	var expression strings.Builder
	expression.WriteString("^")
	for _, char := range pattern {
		switch char {
		case '*':
			expression.WriteString(".*")
		case '?':
			expression.WriteString(".")
		default:
			expression.WriteString(regexp.QuoteMeta(string(char)))
		}
	}
	expression.WriteString("$")
	matched, err := regexp.MatchString(expression.String(), name)
	return err == nil && matched
}

func pathMatches(name, pattern string) bool {
	return fnmatch(name, pattern) || fnmatch(name, "*/"+pattern)
}

// affectedFlows mirrors scripts/e2e.py: the flows a set of changed files can affect.
func affectedFlows(changed []string, areas areaMap, available []string) []string {
	if len(changed) == 0 {
		return available
	}
	chosen := map[string]bool{}
	for _, file := range changed {
		if strings.HasPrefix(file, "e2e/flows/") {
			chosen[path.Base(file)] = true
			continue
		}
		ignored := false
		for _, pattern := range areas.Ignore {
			ignored = ignored || pathMatches(file, pattern)
		}
		if ignored {
			continue
		}
		mapped := false
		for _, area := range areas.Areas {
			hit := false
			for _, pattern := range area.Paths {
				hit = hit || pathMatches(file, pattern)
			}
			if !hit {
				continue
			}
			mapped = true
			for _, pattern := range area.Flows {
				for _, flow := range available {
					if fnmatch(flow, pattern) {
						chosen[flow] = true
					}
				}
			}
		}
		if !mapped {
			for _, prefix := range appSources {
				if strings.HasPrefix(file, prefix) {
					return available
				}
			}
		}
	}
	out := []string{}
	for _, flow := range available {
		if chosen[flow] {
			out = append(out, flow)
		}
	}
	return out
}

// PlanJourneys picks the fixture states a change can affect, most-covered first.
// With no changed files it plans the whole surface. Flows sharing a state become
// one journey, because exploring the same state twice finds nothing new.
func PlanJourneys(repo string, changed []string, limit int) ([]Journey, error) {
	raw, err := os.ReadFile(filepath.Join(repo, "e2e/areas.json"))
	if err != nil {
		return nil, err
	}
	var areas areaMap
	if err := json.Unmarshal(raw, &areas); err != nil {
		return nil, fmt.Errorf("reading e2e/areas.json: %w", err)
	}
	paths, err := filepath.Glob(filepath.Join(repo, "e2e/flows/*.js"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	available := make([]string, 0, len(paths))
	for _, file := range paths {
		available = append(available, filepath.Base(file))
	}
	if len(changed) > 0 {
		if changed = withoutServer(repo, changed); len(changed) == 0 {
			return []Journey{}, nil // every change is server code a fixture run never builds
		}
	}
	journeys, err := journeysFor(repo, affectedFlows(changed, areas, available))
	if err != nil {
		return nil, err
	}
	if len(changed) > 0 {
		journeys, err = addLinked(repo, changed, journeys, func() ([]Journey, error) { return journeysFor(repo, available) })
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(journeys, func(i, j int) bool {
		if len(journeys[i].Flows) != len(journeys[j].Flows) {
			return len(journeys[i].Flows) > len(journeys[j].Flows)
		}
		return journeys[i].ID < journeys[j].ID
	})
	if limit > 0 && len(journeys) > limit {
		journeys = journeys[:limit]
	}
	return journeys, nil
}

// journeysFor groups flows into journeys: flows sharing a fixture state are one.
func journeysFor(repo string, flows []string) ([]Journey, error) {
	states := map[string]*Journey{}
	for _, flow := range flows {
		source, err := os.ReadFile(filepath.Join(repo, "e2e/flows", flow))
		if err != nil {
			return nil, err
		}
		first, _, _ := strings.Cut(string(source), "\n")
		match := fixtureLine.FindStringSubmatch(strings.TrimSpace(first))
		if match == nil {
			continue
		}
		flags := strings.Fields(match[1])
		state := stateFlags(flags)
		if state == nil {
			continue // only the E2E runner can start these
		}
		key := strings.Join(state, " ")
		journey, seen := states[key]
		if !seen {
			journey = &Journey{ID: strings.TrimSuffix(flow, ".js"), Flags: state}
			states[key] = journey
		}
		journey.Flows = append(journey.Flows, flow)
	}
	journeys := make([]Journey, 0, len(states))
	for _, journey := range states {
		journeys = append(journeys, *journey)
	}
	return journeys, nil
}

// stateFlags turns a flow's fixture line into run.sh arguments, or nil when the
// state isn't a plain fixture run. Avatars stay still so that idle checks measure
// the app rather than its characters.
func stateFlags(flags []string) []string {
	state := []string{}
	fixture, still := false, false
	for _, flag := range flags {
		switch flag {
		case "--verify-fixture":
			fixture = true
			continue
		case "--verify-still":
			still = true
		}
		state = append(state, flag)
	}
	if !fixture {
		return nil
	}
	if !still {
		state = append(state, "--verify-still")
	}
	return state
}

// Shard keeps the journeys of one CI shard, numbered from 1.
func Shard(journeys []Journey, index, total int) []Journey {
	out := []Journey{}
	for position, journey := range journeys {
		if position%total == index-1 {
			out = append(out, journey)
		}
	}
	return out
}

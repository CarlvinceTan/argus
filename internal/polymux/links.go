package polymux

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The area map is written by hand, so it forgets things: on Polymux it skipped
// New Bot for a change to the ShellView that opens it. An approved baseline
// records what a journey actually saw, so a changed source that contains a
// label a journey shows links the two, whatever the map says. Links only ever
// add journeys.
const maxLinkedJourneys = 3 // a label most journeys show (a toolbar) says nothing

var swiftLiteral = regexp.MustCompile(`"([^"\\]{4,80})"`)

// linkedJourneys maps each journey a change links to the evidence for it.
func linkedJourneys(repo string, changed []string) map[string]string {
	sources := []string{}
	for _, file := range changed {
		if strings.HasSuffix(file, ".swift") {
			sources = append(sources, file)
		}
	}
	if len(sources) == 0 {
		return nil
	}
	baselines, _ := filepath.Glob(filepath.Join(repo, "e2e/baselines/*.json"))
	shownBy := map[string][]string{}
	for _, file := range baselines {
		id := strings.TrimSuffix(filepath.Base(file), ".json")
		for label := range screenLabels(repo, id) {
			shownBy[label] = append(shownBy[label], id)
		}
	}
	links := map[string]string{}
	for _, file := range sources {
		source, err := os.ReadFile(filepath.Join(repo, file))
		if err != nil {
			continue // deleted: the area rules still decide
		}
		for _, match := range swiftLiteral.FindAllStringSubmatch(string(source), -1) {
			label := match[1]
			journeys := shownBy[label]
			// One word ("Done", "Close") is too common to mean a dependency.
			if len(journeys) == 0 || len(journeys) > maxLinkedJourneys || !strings.Contains(label, " ") {
				continue
			}
			for _, id := range journeys {
				if _, seen := links[id]; !seen {
					links[id] = fmt.Sprintf("%q in %s", label, path.Base(file))
				}
			}
		}
	}
	return links
}

// addLinked adds the journeys a change links to that the area rules left out.
func addLinked(repo string, changed []string, chosen []Journey, all func() ([]Journey, error)) ([]Journey, error) {
	links := linkedJourneys(repo, changed)
	if len(links) == 0 {
		return chosen, nil
	}
	have := map[string]bool{}
	for _, journey := range chosen {
		have[strings.Join(journey.Flags, " ")] = true
	}
	everything, err := all()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(links))
	for id := range links {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, journey := range everything {
		why, linked := links[journey.ID]
		if key := strings.Join(journey.Flags, " "); linked && !have[key] {
			journey.Linked = why
			have[key] = true
			chosen = append(chosen, journey)
		}
	}
	return chosen, nil
}

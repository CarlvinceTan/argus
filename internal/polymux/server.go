package polymux

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Journeys run on fixtures, and scripts/run.sh neither builds nor starts the Go
// server for a fixture run. So a change confined to the server's sources can't
// reach a journey, even though the area rules (which mirror scripts/e2e.py and
// also plan real-server flows) treat it as touching everything. The sources are
// read from run.sh's own fingerprint line, so they follow the script.
var (
	fingerprintLine = regexp.MustCompile(`(?m)^fingerprint (.+server\.stamp.*)$`)
	rootPath        = regexp.MustCompile(`"\$ROOT/([^"]+)"`)
)

// serverSources lists what run.sh builds the server from, or nil when it can't
// tell (then nothing is treated as server-only).
func serverSources(repo string) []string {
	raw, err := os.ReadFile(filepath.Join(repo, "scripts/run.sh"))
	if err != nil {
		return nil
	}
	match := fingerprintLine.FindSubmatch(raw)
	if match == nil {
		return nil
	}
	var sources []string
	for _, path := range rootPath.FindAllSubmatch(match[1], -1) {
		sources = append(sources, string(path[1]))
	}
	return sources
}

// withoutServer drops changes a fixture run never builds.
func withoutServer(repo string, changed []string) []string {
	sources := serverSources(repo)
	if len(sources) == 0 {
		return changed
	}
	kept := []string{}
	for _, file := range changed {
		server := false
		for _, source := range sources {
			server = server || file == source || strings.HasPrefix(file, source+"/")
		}
		if !server {
			kept = append(kept, file)
		}
	}
	return kept
}

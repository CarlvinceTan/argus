package bench

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

var historyHeader = []string{"date", "argus_commit", "host", "full_run_wall_seconds", "per_lane_seconds", "bugs_caught", "bugs_planted", "false_positives", "flaky_items", "go_loc", "moving_parts", "model_calls", "cost_usd", "reason"}

// Reconstruct accepted baselines without rewriting history or letting an
// unexplained regression become the next baseline. Timing is host-specific;
// catch rate and false positives remain comparable across hosts.
func lastHistory(name, host string) (*Summary, error) {
	file, err := os.Open(name)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := csv.NewReader(file)
	reader.Comma = '\t'
	reader.FieldsPerRecord = len(historyHeader)
	header, err := reader.Read()
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for i := range header {
		if header[i] != historyHeader[i] {
			return nil, fmt.Errorf("history header mismatch")
		}
	}
	var last *Summary
	walls := map[string]float64{}
	for {
		row, err := reader.Read()
		if err == io.EOF {
			if last != nil {
				last.WallSeconds = walls[host]
			}
			return last, nil
		}
		if err != nil {
			return nil, err
		}
		s := &Summary{Host: row[2], Reason: row[13]}
		s.WallSeconds, err = strconv.ParseFloat(row[3], 64)
		if err != nil {
			return nil, err
		}
		s.Caught, err = strconv.Atoi(row[5])
		if err != nil {
			return nil, err
		}
		s.FalsePositives, err = strconv.Atoi(row[7])
		if err != nil {
			return nil, err
		}
		var baseline *Summary
		if last != nil {
			copy := *last
			copy.WallSeconds = walls[s.Host]
			baseline = &copy
		}
		if len(regressions(*s, baseline)) > 0 && strings.TrimSpace(s.Reason) == "" {
			continue
		}
		walls[s.Host] = s.WallSeconds
		last = s
	}
}
func appendHistory(name string, s Summary) error {
	file, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer file.Close()
	// Serialize the header and row so concurrent benchmarks never interleave.
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	info, err := file.Stat()
	if err != nil {
		return err
	}
	lanes, err := json.Marshal(s.LaneSeconds)
	if err != nil {
		return err
	}
	flaky, err := json.Marshal(s.Flaky)
	if err != nil {
		return err
	}
	writer := csv.NewWriter(file)
	writer.Comma = '\t'
	if info.Size() == 0 {
		if err = writer.Write(historyHeader); err != nil {
			return err
		}
	}
	row := []string{s.Date, s.ArgusCommit, s.Host, strconv.FormatFloat(s.WallSeconds, 'f', 6, 64), string(lanes), strconv.Itoa(s.Caught), strconv.Itoa(s.Planted), strconv.Itoa(s.FalsePositives), string(flaky), strconv.Itoa(s.GoLOC), strconv.Itoa(s.MovingParts), strconv.Itoa(s.ModelCalls), strconv.FormatFloat(s.CostUSD, 'f', 6, 64), s.Reason}
	if err = writer.Write(row); err != nil {
		return err
	}
	writer.Flush()
	if err = writer.Error(); err != nil {
		return err
	}
	return file.Sync()
}

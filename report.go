package refbench

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

// SchemaVersion is the version of docs/perf.json.
const SchemaVersion = 1

// Status of a report.
const (
	// StatusMeasured: every row has rounds; Valid says whether all were under the gate.
	StatusMeasured = "measured"
	// StatusDeferred: timing was not run (the load gate, or an allocation-only
	// recording); rows carry allocation counts and no verdict.
	StatusDeferred = "deferred"
)

// Round is one sample per side, with the host's load around it.
type Round struct {
	LoadBefore float64 `json:"load_before"`
	LoadAfter  float64 `json:"load_after"`
	// GoFirst is true when the Go side ran before the reference in this round.
	GoFirst bool `json:"go_first"`
	// GoNs and RefNs are nanoseconds per call.
	GoNs  float64 `json:"go_ns"`
	RefNs float64 `json:"ref_ns"`
}

// Row is one case's result.
type Row struct {
	Name      string  `json:"name"`
	Func      string  `json:"func"`
	Reference string  `json:"reference"`
	Parity    string  `json:"parity"`
	Allocs    float64 `json:"allocs"`
	GoIters   int     `json:"go_iters,omitempty"`
	RefIters  int     `json:"ref_iters,omitempty"`
	Rounds    []Round `json:"rounds,omitempty"`

	// Derived from Rounds by Finish; never trusted from a file.
	Go      Summary `json:"go"`
	Ref     Summary `json:"ref"`
	Ratio   float64 `json:"ratio"`
	Verdict string  `json:"verdict,omitempty"`
}

// LoadStats is the min, median and max of the 1-minute load over all rounds.
type LoadStats struct {
	Min    float64 `json:"min"`
	Median float64 `json:"median"`
	Max    float64 `json:"max"`
}

// Report is the content of docs/perf.json.
type Report struct {
	Schema  int    `json:"schema"`
	Status  string `json:"status"`
	Date    string `json:"date"`
	Commit  string `json:"commit"`
	Repo    string `json:"repo"`
	Go      string `json:"go_version"`
	Machine string `json:"machine"`
	// RefVersions: numpy, scipy, blas, compiler, ... as the workers report them.
	RefVersions map[string]string `json:"ref_versions,omitempty"`
	MaxLoad     float64           `json:"max_load"`
	Load        LoadStats         `json:"load"`
	// Valid is true when every round, before and after, was at or below MaxLoad.
	Valid bool  `json:"valid"`
	Rows  []Row `json:"rows"`
}

// Finish recomputes every derived field (summaries, ratio, verdict, load
// statistics) from the rounds.
func (r *Report) Finish() {
	var loads []float64
	for i := range r.Rows {
		row := &r.Rows[i]
		var g, f []float64
		for _, rd := range row.Rounds {
			g = append(g, rd.GoNs)
			f = append(f, rd.RefNs)
			loads = append(loads, rd.LoadBefore, rd.LoadAfter)
		}
		row.Go, row.Ref, row.Ratio, row.Verdict = Summary{}, Summary{}, 0, ""
		if len(g) == 0 {
			continue
		}
		row.Go, row.Ref = Summarize(g), Summarize(f)
		row.Ratio = row.Go.Median / row.Ref.Median
		row.Verdict = Judge(row.Go, row.Ref)
	}
	r.Load = LoadStats{}
	if len(loads) > 0 {
		s := Summarize(loads)
		r.Load = LoadStats{Min: Quantile(loads, 0), Median: s.Median, Max: Quantile(loads, 1)}
	}
}

// Write encodes r as indented JSON with a trailing newline, after Finish.
func (r *Report) Write(w io.Writer) error {
	r.Finish()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// maxReportBytes bounds what ReadReport accepts.
const maxReportBytes = 64 << 20

// ReadReport decodes and validates a perf.json: strict fields, the known
// schema, finite non-negative numbers, rounds only in a measured report. The
// derived fields are recomputed.
func ReadReport(rd io.Reader) (*Report, error) {
	b, err := io.ReadAll(io.LimitReader(rd, maxReportBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxReportBytes {
		return nil, errors.New("perf.json: larger than 64 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r Report
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("perf.json: %w", err)
	}
	if dec.More() {
		return nil, errors.New("perf.json: trailing data after the object")
	}
	if r.Schema != SchemaVersion {
		return nil, fmt.Errorf("perf.json: schema %d, this build reads %d", r.Schema, SchemaVersion)
	}
	if r.Status != StatusMeasured && r.Status != StatusDeferred {
		return nil, fmt.Errorf("perf.json: unknown status %q", r.Status)
	}
	if !finiteNonNeg(r.MaxLoad) {
		return nil, errors.New("perf.json: max_load is not a finite non-negative number")
	}
	seen := map[string]bool{}
	for i, row := range r.Rows {
		if err := checkRow(row, r.Status); err != nil {
			return nil, fmt.Errorf("perf.json: row %d: %w", i, err)
		}
		if seen[row.Name] {
			return nil, fmt.Errorf("perf.json: row %q appears twice", row.Name)
		}
		seen[row.Name] = true
	}
	r.Finish()
	return &r, nil
}

func finiteNonNeg(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) && x >= 0 }

func checkRow(row Row, status string) error {
	switch {
	case row.Name == "" || row.Func == "":
		return errors.New("name and func are required")
	case !finiteNonNeg(row.Allocs):
		return errors.New("allocs is not a finite non-negative number")
	case status == StatusDeferred && len(row.Rounds) > 0:
		return errors.New("a deferred report has no rounds")
	case status == StatusMeasured && len(row.Rounds) == 0:
		return errors.New("a measured report needs rounds")
	}
	for _, rd := range row.Rounds {
		if !finiteNonNeg(rd.LoadBefore) || !finiteNonNeg(rd.LoadAfter) {
			return errors.New("a round's load is not a finite non-negative number")
		}
		if !finiteNonNeg(rd.GoNs) || !finiteNonNeg(rd.RefNs) || rd.RefNs == 0 {
			return errors.New("a round's sample is not a finite positive time")
		}
	}
	return nil
}

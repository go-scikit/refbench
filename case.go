// Package refbench measures a Go function against its reference (NumPy, SciPy,
// a C library) on one core, in interleaved rounds, gated on the host's load.
//
// Every repository registers its cases in Go. A Case names the public function
// it measures; Run alternates the two sides round by round, reads the 1-minute
// load before every round and records it; the Report is written as
// docs/perf.json and docs/perf-table.md. The CI mode (RunCI) needs no
// reference and no timing: it compares allocation counts with perf.json,
// checks that the table is what the JSON generates, and checks that every
// public function has a case or a named exemption.
package refbench

import (
	"errors"
	"fmt"
	"time"
)

// Parity says how a result is validated against the reference.
const (
	ParityExact     = "exact"
	ParityTolerance = "tolerance"
	ParityNone      = "none"
)

// Timer runs the timed body iters times and returns the elapsed time, measured
// inside its own process: a pipe or a process start is never in the interval.
type Timer func(iters int) (time.Duration, error)

// Case is one measured row.
type Case struct {
	// Name is unique in a registry; it is the row's label and the issue key.
	Name string
	// Func is the public function the case measures, in the form the coverage
	// check reads: "Orient2D", "Type.Method", or "sub/pkg.Func" below the root.
	Func string
	// Reference names the reference, e.g. "predicates.c" or "scipy.special.beta".
	Reference string
	// Parity is ParityExact, ParityTolerance or ParityNone.
	Parity string
	// Go runs the Go body iters times. It is timed by Run.
	Go func(iters int)
	// AllocIters is the number of calls the allocation count averages over (0
	// means 1). A body that cycles through inputs sets it to the cycle length,
	// so that every input is counted and the result is exact.
	AllocIters int
	// Ref times the reference body, inside the reference's process. Nil for a
	// case only used by the allocation check.
	Ref Timer
}

// Validate checks a registry: unique names, the required fields, a known parity.
func Validate(cases []Case) error {
	seen := map[string]bool{}
	for i, c := range cases {
		switch {
		case c.Name == "":
			return fmt.Errorf("case %d has no name", i)
		case seen[c.Name]:
			return fmt.Errorf("case %q is registered twice", c.Name)
		case c.Func == "":
			return fmt.Errorf("case %q names no public function", c.Name)
		case c.Go == nil:
			return fmt.Errorf("case %q has no Go body", c.Name)
		case c.Parity != ParityExact && c.Parity != ParityTolerance && c.Parity != ParityNone:
			return fmt.Errorf("case %q: unknown parity %q", c.Name, c.Parity)
		}
		seen[c.Name] = true
	}
	return nil
}

var errNoTime = errors.New("a calibration run took no measurable time")

// Calibrate returns the iteration count for which one run of t lasts at least
// minDur: it doubles from 1, then scales by the last measurement.
func Calibrate(t Timer, minDur time.Duration) (int, error) {
	const maxIters = 1 << 40
	iters := 1
	for {
		d, err := t(iters)
		if err != nil {
			return 0, err
		}
		if d >= minDur {
			return iters, nil
		}
		next := iters * 2
		if d > 0 {
			if n := int(float64(iters) * float64(minDur) / float64(d) * 1.1); n > next {
				next = n
			}
		}
		if next > maxIters || next < iters {
			return 0, errNoTime
		}
		iters = next
	}
}

// Package rbtest is the glue between refbench and `go test`, written once so
// that no repository repeats it. A repository's perf_test.go is
//
//	func TestPerf(t *testing.T) {
//		rbtest.Perf(t, refbench.CIConfig{Root: "..", Cases: cases}, refbench.Options{Repo: "go-x/y"})
//	}
//
// and then
//
//	go test -run TestPerf .                         the CI mode: allocations, table, denominator
//	go test -run TestPerf -cpu 1 -perf.run .        measure, write docs/perf.json and docs/perf-table.md
//	go test -run TestPerf -perf.record .            allocation counts only (status: deferred)
//
// Timing runs only when the load gate opens. With -perf.run a gate that does
// not open (refused, or still closed after -perf.wait) fails the test with the
// reason and writes nothing: an asked-for measurement that did not happen is
// not a pass. Without -perf.run the test never times anything.
package rbtest

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"testing"
	"time"

	"github.com/go-scikit/refbench"
)

var (
	runFlag     = flag.Bool("perf.run", false, "measure against the reference and write perf.json and perf-table.md")
	recordFlag  = flag.Bool("perf.record", false, "record allocation counts only (a deferred report)")
	roundsFlag  = flag.Int("perf.rounds", 7, "rounds per case")
	minTimeFlag = flag.Duration("perf.mintime", 100*time.Millisecond, "minimum duration of a sample")
	maxLoadFlag = flag.Float64("perf.maxload", refbench.DefaultMaxLoad, "the 1-minute load gate")
	onLoadFlag  = flag.String("perf.onload", refbench.OnLoadRefuse, "wait or refuse when the load is at or above the gate")
	waitFlag    = flag.Duration("perf.wait", 30*time.Minute, "how long to wait for the gate with -perf.onload=wait")
	commitFlag  = flag.String("perf.commit", "", "commit recorded in the report")
	machineFlag = flag.String("perf.machine", "", "machine description recorded in the report (default: GOOS/GOARCH, CPUs)")
)

// T is the part of *testing.T that Perf uses.
type T interface {
	Helper()
	Logf(format string, args ...any)
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}

// Perf runs the CI mode, or a measurement when -perf.run or -perf.record is set.
func Perf(t *testing.T, cfg refbench.CIConfig, opt refbench.Options) {
	t.Helper()
	perf(t, cfg, opt)
}

func perf(t T, cfg refbench.CIConfig, opt refbench.Options) {
	t.Helper()
	opt.Rounds, opt.MinTime, opt.MaxLoad = *roundsFlag, *minTimeFlag, *maxLoadFlag
	opt.OnLoad, opt.LoadTimeout = *onLoadFlag, *waitFlag
	opt.Commit, opt.Machine = *commitFlag, *machineFlag
	if *recordFlag && *runFlag {
		t.Fatalf("-perf.run and -perf.record are exclusive")
	}
	switch {
	case *runFlag:
		rep, err := refbench.Run(context.Background(), cfg.Cases, opt)
		if errors.Is(err, refbench.ErrOnLoad) {
			// An explicit timing request that measured nothing is a failure:
			// a skip prints only PASS without -v.
			t.Fatalf("timing requested by -perf.run but deferred, nothing written: %v", err)
		}
		if err != nil {
			t.Fatalf("%v", err)
		}
		write(t, cfg, rep)
	case *recordFlag:
		rep, err := refbench.RecordAllocs(cfg.Cases, opt)
		if err != nil {
			t.Fatalf("%v", err)
		}
		write(t, cfg, rep)
	default:
		if err := refbench.RunCI(cfg); err != nil {
			t.Fatalf("%v", err)
		}
	}
}

// write stores perf.json and the table generated from it, at the paths RunCI reads.
func write(t T, cfg refbench.CIConfig, rep *refbench.Report) {
	t.Helper()
	jsonPath, tablePath := cfg.Paths()
	var b bytes.Buffer
	if err := rep.Write(&b); err != nil {
		t.Fatalf("%v", err)
	}
	if err := os.WriteFile(jsonPath, b.Bytes(), 0o644); err != nil {
		t.Fatalf("%v", err)
	}
	if err := os.WriteFile(tablePath, refbench.Table(rep), 0o644); err != nil {
		t.Fatalf("%v", err)
	}
	t.Logf("wrote %s and %s (status %s, valid %v)", jsonPath, tablePath, rep.Status, rep.Valid)
}

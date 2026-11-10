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
//	go test -run TestPerf -perf.record .            allocation counts only (timings kept)
//	go test -run TestPerf -cpu 1 -perf.resume -perf.onload=wait .   measure only the deferred rows
//
// Timing runs only when the load gate opens. A round that ends above the gate
// is discarded and re-run once the load is back under it (-perf.onload=wait
// -perf.wait=...; -perf.retries caps the re-runs per row). Rows are valid on
// their own: perf.json keeps the valid rows and lists the deferred ones, and the
// test then FAILS ("partial") so that an incomplete run never reads as ok;
// -perf.resume measures only the deferred rows (same commit, Go and reference
// versions) and merges. A run that measured no row at all fails and writes
// nothing: an asked-for measurement that did not happen is not a pass. Without
// -perf.run the test never times anything. -perf.record keeps the recorded
// timings and only updates the allocation counts.
//
// Nothing is written unless the repository root's go.mod names the parent of
// this binary's module (a bench/ module of that repository): a binary of one
// repository run from another's bench/ directory refuses, naming both.
package rbtest

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/go-scikit/refbench"
)

var (
	runFlag     = flag.Bool("perf.run", false, "measure against the reference and write perf.json and perf-table.md")
	resumeFlag  = flag.Bool("perf.resume", false, "measure only the rows perf.json lists as deferred (same commit, Go and reference versions) and merge")
	retriesFlag = flag.Int("perf.retries", refbench.DefaultMaxRetries, "per row, how many rounds that end above the gate are re-run before the row is deferred")
	recordFlag  = flag.Bool("perf.record", false, "record allocation counts only, keeping the recorded timings")
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

// flagSet reports whether a flag was given on the command line; tests replace it.
var flagSet = func(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// buildModule is the module path of the running binary; tests replace it.
var buildModule = func() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return bi.Main.Path
	}
	return ""
}

// modulePath reads the module path of a go.mod file.
func modulePath(file string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module")
		if !ok || rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
			continue
		}
		rest, _, _ = strings.Cut(rest, "//")
		return strings.Trim(strings.TrimSpace(rest), "\"`"), nil
	}
	return "", fmt.Errorf("%s: no module line", file)
}

// checkRoot refuses to touch docs/ of a repository that is not the binary's
// own: the module of cfg.Root's go.mod must be the parent of the binary's
// module (github.com/o/r for github.com/o/r/bench), or the same module.
func checkRoot(cfg refbench.CIConfig) error {
	goMod := filepath.Join(cfg.Root, "go.mod")
	root, err := modulePath(goMod)
	if err != nil {
		return fmt.Errorf("refusing to write docs: cannot read the root module: %w", err)
	}
	self := buildModule()
	if self == "" {
		return errors.New("refusing to write docs: this binary carries no module path")
	}
	if self != root && path.Dir(self) != root {
		return fmt.Errorf("refusing to write docs: this binary's module is %q, but %s declares module %q, which is not its parent (a binary of one repository was run from another's bench directory?)", self, goMod, root)
	}
	return nil
}

// readPrev reads the recorded report; a missing file is (nil, nil).
func readPrev(cfg refbench.CIConfig) (*refbench.Report, error) {
	jsonPath, _ := cfg.Paths()
	b, err := os.ReadFile(jsonPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	rep, err := refbench.ReadReport(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w (fix or delete it)", jsonPath, err)
	}
	return rep, nil
}

func perf(t T, cfg refbench.CIConfig, opt refbench.Options) {
	t.Helper()
	opt.Rounds, opt.MinTime, opt.MaxLoad = *roundsFlag, *minTimeFlag, *maxLoadFlag
	opt.OnLoad, opt.LoadTimeout = *onLoadFlag, *waitFlag
	opt.Commit, opt.Machine, opt.MaxRetries = *commitFlag, *machineFlag, *retriesFlag
	if n := b2i(*recordFlag) + b2i(*runFlag) + b2i(*resumeFlag); n > 1 {
		t.Fatalf("-perf.run, -perf.resume and -perf.record are exclusive")
	}
	if flagSet("perf.wait") && *onLoadFlag != refbench.OnLoadWait {
		t.Fatalf("-perf.wait has no effect without -perf.onload=wait (the default is %s): add -perf.onload=wait", refbench.OnLoadRefuse)
	}
	if *runFlag || *resumeFlag || *recordFlag {
		if err := checkRoot(cfg); err != nil {
			t.Fatalf("%v", err)
		}
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
		failIfPartial(t, rep)
	case *resumeFlag:
		prev, err := readPrev(cfg)
		if err == nil && prev == nil {
			err = errors.New("-perf.resume: no perf.json to resume (run -perf.run first)")
		}
		if err == nil {
			err = refbench.CheckResume(prev, opt)
		}
		if err != nil {
			t.Fatalf("%v", err)
		}
		todo := refbench.Pending(prev, cfg.Cases)
		if len(todo) == 0 {
			t.Logf("-perf.resume: no row is deferred, nothing to measure")
			return
		}
		opt.Only = todo
		rep, err := refbench.Run(context.Background(), cfg.Cases, opt)
		if errors.Is(err, refbench.ErrOnLoad) {
			t.Fatalf("timing requested by -perf.resume but deferred, nothing written: %v", err)
		}
		if err == nil {
			rep, err = refbench.Merge(prev, rep)
		}
		if err != nil {
			t.Fatalf("%v", err)
		}
		write(t, cfg, rep)
		failIfPartial(t, rep)
	case *recordFlag:
		rep, err := refbench.RecordAllocs(cfg.Cases, opt)
		if err != nil {
			t.Fatalf("%v", err)
		}
		prev, err := readPrev(cfg)
		if err != nil {
			t.Fatalf("%v", err)
		}
		write(t, cfg, refbench.MergeAllocs(prev, rep))
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
	if rep.Status == refbench.StatusMeasured {
		t.Logf("%d of %d rows valid, %d retries", rep.ValidRows(), len(rep.Rows), rep.Retries)
	}
}

// failIfPartial fails a timing run that wrote deferred rows: written, but not a
// verdict, so a partial run never reads as ok.
func failIfPartial(t T, rep *refbench.Report) {
	t.Helper()
	if !rep.Valid {
		t.Fatalf("partial: %d of %d rows valid, deferred: %s (the files are written; run again with -perf.resume -perf.onload=wait)",
			rep.ValidRows(), len(rep.Rows), strings.Join(rep.Deferred, ", "))
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

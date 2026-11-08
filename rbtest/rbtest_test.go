package rbtest

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-scikit/refbench"
)

type fatalSentinel struct{ kind, msg string }

// fakeT records the outcome; Fatalf and Skipf stop the call like testing does.
type fakeT struct{ logs []string }

func (*fakeT) Helper()                     {}
func (f *fakeT) Logf(s string, a ...any)   { f.logs = append(f.logs, fmt.Sprintf(s, a...)) }
func (f *fakeT) Fatalf(s string, a ...any) { panic(fatalSentinel{"fatal", fmt.Sprintf(s, a...)}) }
func (f *fakeT) Skipf(s string, a ...any)  { panic(fatalSentinel{"skip", fmt.Sprintf(s, a...)}) }

// outcome runs fn and returns "ok", "fatal: ..." or "skip: ...".
func outcome(fn func()) (res string) {
	defer func() {
		if r := recover(); r != nil {
			s := r.(fatalSentinel)
			res = s.kind + ": " + s.msg
		}
	}()
	fn()
	return "ok"
}

func setFlags(t *testing.T, run, record bool) {
	t.Helper()
	oldRun, oldRec, oldOn, oldRounds, oldMin := *runFlag, *recordFlag, *onLoadFlag, *roundsFlag, *minTimeFlag
	t.Cleanup(func() {
		*runFlag, *recordFlag, *onLoadFlag, *roundsFlag, *minTimeFlag = oldRun, oldRec, oldOn, oldRounds, oldMin
	})
	*runFlag, *recordFlag = run, record
	*roundsFlag, *minTimeFlag = 3, time.Microsecond
}

var sink int

func spin(n int) {
	for i := 0; i < n; i++ {
		sink += i
	}
}

func repo(t *testing.T, ref refbench.Timer) refbench.CIConfig {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "lib.go"), []byte("package lib\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	return refbench.CIConfig{Root: root, Cases: []refbench.Case{{
		Name: "a", Func: "A", Reference: "ref", Parity: refbench.ParityExact, Go: spin, Ref: ref,
	}}}
}

func fixedRef(iters int) (time.Duration, error) { return time.Duration(iters) * time.Microsecond, nil }

func lowLoad() (float64, error) { return 1, nil }

func TestRecordThenCI(t *testing.T) {
	cfg := repo(t, fixedRef)
	setFlags(t, false, true)
	ft := &fakeT{}
	if got := outcome(func() { perf(ft, cfg, refbench.Options{}) }); got != "ok" {
		t.Fatal(got)
	}
	jsonPath, tablePath := cfg.Paths()
	for _, p := range []string{jsonPath, tablePath} {
		if _, err := os.Stat(p); err != nil {
			t.Error(err)
		}
	}
	if len(ft.logs) != 1 || !strings.Contains(ft.logs[0], "status deferred") {
		t.Errorf("logs = %v", ft.logs)
	}
	// The CI mode accepts what was recorded.
	setFlags(t, false, false)
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{}) }); got != "ok" {
		t.Errorf("CI mode: %s", got)
	}
	// And refuses a hand edit.
	if err := os.WriteFile(tablePath, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{}) }); !strings.HasPrefix(got, "fatal: ") {
		t.Errorf("hand edit: %s", got)
	}
}

func TestRun(t *testing.T) {
	cfg := repo(t, fixedRef)
	setFlags(t, true, false)
	*onLoadFlag = refbench.OnLoadRefuse
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: lowLoad}) }); got != "ok" {
		t.Fatal(got)
	}
	jsonPath, _ := cfg.Paths()
	f, err := os.Open(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rep, err := refbench.ReadReport(f)
	if err != nil || rep.Status != refbench.StatusMeasured || !rep.Valid || len(rep.Rows[0].Rounds) != 3 {
		t.Errorf("report = %+v, %v", rep, err)
	}
	// A measured report passes the CI mode too (allocations are exact).
	setFlags(t, false, false)
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{}) }); got != "ok" {
		t.Errorf("CI mode: %s", got)
	}
}

func TestRunGateClosedFailsAndWritesNothing(t *testing.T) {
	cfg := repo(t, fixedRef)
	setFlags(t, true, false)
	*onLoadFlag = refbench.OnLoadRefuse
	busy := func() (float64, error) { return 99, nil }
	got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: busy}) })
	if !strings.HasPrefix(got, "fatal: timing requested by -perf.run but deferred, nothing written") ||
		!strings.Contains(got, "load gate did not open") {
		t.Errorf("got %s", got)
	}
	jsonPath, _ := cfg.Paths()
	if _, err := os.Stat(jsonPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("perf.json written under load: %v", err)
	}
}

// A wait that outlasts the busy period measures; a wait that does not, fails.
func TestRunWaitOpensOrFails(t *testing.T) {
	cfg := repo(t, fixedRef)
	setFlags(t, true, false)
	*onLoadFlag = refbench.OnLoadWait
	calls := 0
	clears := func() (float64, error) {
		calls++
		if calls < 3 {
			return 99, nil
		}
		return 0, nil
	}
	opt := refbench.Options{Load: clears, Poll: time.Millisecond}
	if got := outcome(func() { perf(&fakeT{}, cfg, opt) }); got != "ok" {
		t.Errorf("gate opened within the wait: %s", got)
	}
	cfg = repo(t, fixedRef)
	*waitFlag = 20 * time.Millisecond
	busy := func() (float64, error) { return 99, nil }
	got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: busy, Poll: time.Millisecond}) })
	if !strings.HasPrefix(got, "fatal: timing requested by -perf.run") {
		t.Errorf("wait timed out: %s", got)
	}
}

// Without -perf.run a busy machine changes nothing: the CI mode never times.
func TestCIModeIgnoresLoad(t *testing.T) {
	cfg := repo(t, fixedRef)
	setFlags(t, false, true)
	busy := func() (float64, error) { return 99, nil }
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: busy}) }); got != "ok" {
		t.Fatal(got)
	}
	setFlags(t, false, false)
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: busy}) }); got != "ok" {
		t.Errorf("CI mode under load: %s", got)
	}
}

func TestFailures(t *testing.T) {
	// A reference that fails stops the run.
	cfg := repo(t, func(int) (time.Duration, error) { return 0, errors.New("raised") })
	setFlags(t, true, false)
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: lowLoad}) }); !strings.Contains(got, "raised") {
		t.Errorf("reference error: %s", got)
	}
	// Exclusive flags.
	setFlags(t, true, true)
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{}) }); !strings.Contains(got, "exclusive") {
		t.Errorf("both flags: %s", got)
	}
	// An invalid registry fails the recording.
	setFlags(t, false, true)
	bad := refbench.CIConfig{Root: t.TempDir(), Cases: []refbench.Case{{}}}
	if got := outcome(func() { perf(&fakeT{}, bad, refbench.Options{}) }); !strings.HasPrefix(got, "fatal: ") {
		t.Errorf("invalid registry: %s", got)
	}
	// The CI mode without recorded files fails.
	setFlags(t, false, false)
	if got := outcome(func() { perf(&fakeT{}, repo(t, nil), refbench.Options{}) }); !strings.HasPrefix(got, "fatal: ") {
		t.Errorf("CI without files: %s", got)
	}
}

func TestWriteErrors(t *testing.T) {
	rep := &refbench.Report{Schema: 1, Status: refbench.StatusDeferred, Rows: []refbench.Row{{Name: "a", Func: "A", Allocs: 1}}}
	root := t.TempDir()
	// JSON cannot be created.
	cfg := refbench.CIConfig{Root: root, JSON: filepath.Join(root, "no", "perf.json")}
	if got := outcome(func() { write(&fakeT{}, cfg, rep) }); !strings.HasPrefix(got, "fatal: ") {
		t.Errorf("create: %s", got)
	}
	// JSON cannot be encoded (NaN).
	nan := &refbench.Report{Schema: 1, Status: refbench.StatusDeferred, Rows: []refbench.Row{{Name: "a", Func: "A", Allocs: math.NaN()}}}
	cfg = refbench.CIConfig{Root: root, JSON: filepath.Join(root, "perf.json"), Table: filepath.Join(root, "perf-table.md")}
	if got := outcome(func() { write(&fakeT{}, cfg, nan) }); !strings.HasPrefix(got, "fatal: ") {
		t.Errorf("encode: %s", got)
	}
	// The table cannot be written.
	cfg.Table = filepath.Join(root, "no", "perf-table.md")
	if got := outcome(func() { write(&fakeT{}, cfg, rep) }); !strings.HasPrefix(got, "fatal: ") {
		t.Errorf("table: %s", got)
	}
}

func TestPerfWrapper(t *testing.T) {
	// Perf is perf over a real *testing.T: the CI mode on a recorded repository.
	cfg := repo(t, fixedRef)
	setFlags(t, false, true)
	Perf(t, cfg, refbench.Options{})
	setFlags(t, false, false)
	Perf(t, cfg, refbench.Options{})
}

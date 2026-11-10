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
	oldResume, oldCommit, oldWait, oldFlagSet, oldModule := *resumeFlag, *commitFlag, *waitFlag, flagSet, buildModule
	t.Cleanup(func() {
		*runFlag, *recordFlag, *onLoadFlag, *roundsFlag, *minTimeFlag = oldRun, oldRec, oldOn, oldRounds, oldMin
		*resumeFlag, *commitFlag, *waitFlag, flagSet, buildModule = oldResume, oldCommit, oldWait, oldFlagSet, oldModule
	})
	*runFlag, *recordFlag, *resumeFlag = run, record, false
	*roundsFlag, *minTimeFlag = 3, time.Microsecond
	flagSet = func(string) bool { return false }
	buildModule = func() string { return "example.com/lib/bench" }
}

var sink int

func spin(n int) {
	for i := 0; i < n; i++ {
		sink += i
	}
}

func repo(t *testing.T, ref refbench.Timer) refbench.CIConfig {
	t.Helper()
	return repoOf(t, ref, "a")
}

// repoOf is a repository (module example.com/lib) with one case per name; case
// "a" measures A, the others B.
func repoOf(t *testing.T, ref refbench.Timer, names ...string) refbench.CIConfig {
	t.Helper()
	root := t.TempDir()
	lib := "package lib\nfunc A() {}\n"
	if len(names) > 1 {
		lib += "func B() {}\n"
	}
	files := map[string]string{"lib.go": lib, "go.mod": "module example.com/lib // the repo\n\ngo 1.27\n"}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := refbench.CIConfig{Root: root}
	for _, n := range names {
		fn := "B"
		if n == "a" {
			fn = "A"
		}
		cfg.Cases = append(cfg.Cases, refbench.Case{Name: n, Func: fn, Reference: "ref", Parity: refbench.ParityExact, Go: spin, Ref: ref})
	}
	return cfg
}

func readJSON(t *testing.T, cfg refbench.CIConfig) *refbench.Report {
	t.Helper()
	jsonPath, _ := cfg.Paths()
	f, err := os.Open(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rep, err := refbench.ReadReport(f)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// seq returns the readings in turn, then the last one forever.
func seq(vals ...float64) func() (float64, error) {
	i := 0
	return func() (float64, error) {
		v := vals[min(i, len(vals)-1)]
		i++
		return v, nil
	}
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
	bad := refbench.CIConfig{Root: repo(t, nil).Root, Cases: []refbench.Case{{}}}
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

// A closed gate after the first row: the run is partial (written, and the test
// fails); -perf.resume measures only the deferred row and keeps the valid one.
func TestPartialThenResume(t *testing.T) {
	cfg := repoOf(t, fixedRef, "a", "b")
	setFlags(t, true, false)
	*onLoadFlag = refbench.OnLoadRefuse
	// Row a: 3 rounds of (gate, after) at load 1; row b: the gate is closed.
	got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: seq(1, 1, 1, 1, 1, 1, 99)}) })
	if !strings.HasPrefix(got, "fatal: partial: 1 of 2 rows valid, deferred: b") {
		t.Fatalf("got %s", got)
	}
	rep := readJSON(t, cfg)
	if rep.Valid || len(rep.Deferred) != 1 || rep.Deferred[0] != "b" || len(rep.Rows[0].Rounds) != 3 || len(rep.Rows[1].Rounds) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	_, tablePath := cfg.Paths()
	tbl, err := os.ReadFile(tablePath)
	if err != nil || !strings.Contains(string(tbl), "partial: 1 of 2 rows valid") {
		t.Errorf("table: %v\n%s", err, tbl)
	}
	// The CI mode accepts a partial report (allocations are exact).
	setFlags(t, false, false)
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{}) }); got != "ok" {
		t.Errorf("CI mode: %s", got)
	}
	// Resume: same commit, Go and references; only b is timed (a's rounds stay).
	setFlags(t, false, false)
	*resumeFlag = true
	*onLoadFlag = refbench.OnLoadWait
	timed := map[string]int{}
	cfg2 := cfg
	cfg2.Cases = nil
	for _, c := range cfg.Cases {
		c := c
		g := c.Go
		c.Go = func(n int) { timed[c.Name]++; g(n) }
		cfg2.Cases = append(cfg2.Cases, c)
	}
	if got := outcome(func() { perf(&fakeT{}, cfg2, refbench.Options{Load: lowLoad}) }); got != "ok" {
		t.Fatal(got)
	}
	if timed["a"] != 0 || timed["b"] == 0 {
		t.Errorf("calls: a %d (must be 0), b %d", timed["a"], timed["b"])
	}
	done := readJSON(t, cfg)
	if !done.Valid || len(done.Deferred) != 0 || len(done.Rows[1].Rounds) != 3 {
		t.Fatalf("resumed = %+v", done)
	}
	for i, rd := range done.Rows[0].Rounds {
		if rd != rep.Rows[0].Rounds[i] {
			t.Errorf("row a round %d changed: %+v -> %+v", i, rep.Rows[0].Rounds[i], rd)
		}
	}
	// Complete: a further resume has nothing to do.
	ft := &fakeT{}
	if got := outcome(func() { perf(ft, cfg2, refbench.Options{Load: lowLoad}) }); got != "ok" || len(ft.logs) != 1 || !strings.Contains(ft.logs[0], "no row is deferred") {
		t.Errorf("complete: %s %v", got, ft.logs)
	}
}

func TestResumeRefusals(t *testing.T) {
	cfg := repoOf(t, fixedRef, "a", "b")
	setFlags(t, false, false)
	*resumeFlag = true
	*onLoadFlag = refbench.OnLoadWait
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: lowLoad}) }); !strings.Contains(got, "no perf.json to resume") {
		t.Errorf("no file: %s", got)
	}
	// A deferred (allocation-only) report cannot be resumed.
	setFlags(t, false, true)
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{}) }); got != "ok" {
		t.Fatal(got)
	}
	setFlags(t, false, false)
	*resumeFlag = true
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: lowLoad}) }); !strings.Contains(got, "not measured") {
		t.Errorf("deferred report: %s", got)
	}
	// A measured, partial report at another commit.
	setFlags(t, true, false)
	*onLoadFlag = refbench.OnLoadRefuse
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: seq(1, 1, 1, 1, 1, 1, 99)}) }); !strings.Contains(got, "partial") {
		t.Fatal(got)
	}
	setFlags(t, false, false)
	*resumeFlag = true
	*onLoadFlag = refbench.OnLoadWait
	*commitFlag = "other"
	got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: lowLoad}) })
	if !strings.Contains(got, "commit") || !strings.Contains(got, `"other"`) {
		t.Errorf("other commit: %s", got)
	}
	*commitFlag = ""
	// A reference that fails during the resume stops it.
	bad := cfg
	bad.Cases = nil
	for _, c := range cfg.Cases {
		c.Ref = func(int) (time.Duration, error) { return 0, errors.New("raised") }
		bad.Cases = append(bad.Cases, c)
	}
	if got := outcome(func() { perf(&fakeT{}, bad, refbench.Options{Load: lowLoad}) }); !strings.Contains(got, "raised") {
		t.Errorf("reference error: %s", got)
	}
	// A gate that stays closed during the resume: nothing written, file unchanged.
	jsonPath, _ := cfg.Paths()
	before, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	*waitFlag = 10 * time.Millisecond
	busy := func() (float64, error) { return 99, nil }
	got = outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: busy, Poll: time.Millisecond}) })
	after, _ := os.ReadFile(jsonPath)
	if !strings.HasPrefix(got, "fatal: timing requested by -perf.resume") || string(before) != string(after) {
		t.Errorf("busy resume: %s", got)
	}
}

// -perf.record updates the allocation counts and keeps the recorded timings.
func TestRecordKeepsTimings(t *testing.T) {
	cfg := repo(t, fixedRef)
	setFlags(t, true, false)
	*onLoadFlag = refbench.OnLoadRefuse
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: lowLoad}) }); got != "ok" {
		t.Fatal(got)
	}
	before := readJSON(t, cfg)
	setFlags(t, false, true)
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{}) }); got != "ok" {
		t.Fatal(got)
	}
	after := readJSON(t, cfg)
	if after.Status != refbench.StatusMeasured || len(after.Rows[0].Rounds) != 3 || after.Rows[0].Go != before.Rows[0].Go || !after.Valid {
		t.Errorf("timings lost: %+v", after)
	}
	// A recording over a corrupt file refuses instead of wiping it.
	jsonPath, _ := cfg.Paths()
	if err := os.WriteFile(jsonPath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{}) }); !strings.Contains(got, "fix or delete it") {
		t.Errorf("corrupt: %s", got)
	}
	// An unreadable path is an error too.
	if err := os.Remove(jsonPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(jsonPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{}) }); !strings.HasPrefix(got, "fatal: ") {
		t.Errorf("directory: %s", got)
	}
}

func TestWaitWithoutOnLoadWait(t *testing.T) {
	cfg := repo(t, fixedRef)
	setFlags(t, true, false)
	*onLoadFlag = refbench.OnLoadRefuse
	flagSet = func(name string) bool { return name == "perf.wait" }
	got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: lowLoad}) })
	if !strings.Contains(got, "-perf.wait") || !strings.Contains(got, "-perf.onload=wait") {
		t.Errorf("got %s", got)
	}
	*onLoadFlag = refbench.OnLoadWait
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: lowLoad}) }); got != "ok" {
		t.Errorf("with onload=wait: %s", got)
	}
}

// A binary of another repository, run from its bench directory, writes nothing.
func TestRootGuard(t *testing.T) {
	cfg := repo(t, fixedRef)
	jsonPath, tablePath := cfg.Paths()
	buildModule = func() string { return "example.com/other/bench" }
	for _, run := range []struct{ run, record bool }{{true, false}, {false, true}} {
		setFlags(t, run.run, run.record)
		buildModule = func() string { return "example.com/other/bench" }
		got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{Load: lowLoad}) })
		if !strings.HasPrefix(got, "fatal: refusing to write docs") || !strings.Contains(got, `"example.com/other/bench"`) || !strings.Contains(got, `"example.com/lib"`) {
			t.Errorf("run=%v: %s", run.run, got)
		}
		for _, p := range []string{jsonPath, tablePath} {
			if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s written: %v", p, err)
			}
		}
	}
	// No build info, no go.mod: refused as well.
	setFlags(t, false, true)
	buildModule = func() string { return "" }
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{}) }); !strings.Contains(got, "no module path") {
		t.Errorf("no build info: %s", got)
	}
	setFlags(t, false, true)
	cfg.Root = t.TempDir()
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{}) }); !strings.Contains(got, "cannot read the root module") {
		t.Errorf("no go.mod: %s", got)
	}
	// The module itself (no bench/ module) is allowed, and so is its bench/.
	for _, mod := range []string{"example.com/lib", "example.com/lib/bench"} {
		cfg = repo(t, fixedRef)
		setFlags(t, false, true)
		buildModule = func() string { return mod }
		if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{}) }); got != "ok" {
			t.Errorf("%s: %s", mod, got)
		}
	}
	// The CI mode writes nothing and is not guarded.
	setFlags(t, false, false)
	buildModule = func() string { return "example.com/other/bench" }
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{}) }); got != "ok" {
		t.Errorf("CI mode: %s", got)
	}
}

func TestModulePath(t *testing.T) {
	dir := t.TempDir()
	for in, want := range map[string]string{
		"module a.b/c\n":                       "a.b/c",
		"// x\nmodule \"a.b/c\" // hi\ngo 1\n": "a.b/c",
		"modulefoo\nmodule\ta/b\n":             "a/b",
	} {
		p := filepath.Join(dir, "go.mod")
		if err := os.WriteFile(p, []byte(in), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := modulePath(p); err != nil || got != want {
			t.Errorf("%q: %q, %v", in, got, err)
		}
	}
	p := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(p, []byte("go 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := modulePath(p); err == nil {
		t.Error("no module line accepted")
	}
}

func TestExclusiveWithResume(t *testing.T) {
	cfg := repo(t, fixedRef)
	setFlags(t, true, false)
	*resumeFlag = true
	if got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{}) }); !strings.Contains(got, "exclusive") {
		t.Errorf("got %s", got)
	}
}

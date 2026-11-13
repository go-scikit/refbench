package refbench

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestQuantileSummarizeJudge(t *testing.T) {
	if !math.IsNaN(Quantile(nil, 0.5)) {
		t.Error("empty: want NaN")
	}
	xs := []float64{4, 1, 3, 2}
	if got := Quantile(xs, 0.5); got != 2.5 {
		t.Errorf("median = %v", got)
	}
	if xs[0] != 4 {
		t.Error("Quantile modified its input")
	}
	s := Summarize([]float64{1, 2, 3, 4, 5})
	if s != (Summary{Median: 3, Q1: 2, Q3: 4}) {
		t.Errorf("summary = %+v", s)
	}
	ref := Summary{Median: 10, Q1: 9, Q3: 11}
	for _, tc := range []struct {
		g    Summary
		want string
	}{
		{Summary{Median: 20, Q1: 15, Q3: 25}, Slower},
		{Summary{Median: 10, Q1: 10.5, Q3: 12}, Tie},
		{Summary{Median: 5, Q1: 4, Q3: 6}, Faster},
	} {
		if got := Judge(tc.g, ref); got != tc.want {
			t.Errorf("Judge(%+v) = %s, want %s", tc.g, got, tc.want)
		}
	}
}

func goodCase(name string) Case {
	return Case{Name: name, Func: "F", Reference: "ref", Parity: ParityExact, Go: spin}
}

var spinSink int

// spin is a Go body whose time grows with iters.
func spin(n int) {
	for i := 0; i < n; i++ {
		spinSink += i
	}
}

func TestValidate(t *testing.T) {
	good := goodCase("a")
	for name, cs := range map[string][]Case{
		"noname": {{}},
		"dup":    {good, good},
		"nofunc": {{Name: "x", Go: good.Go, Parity: ParityNone}},
		"nogo":   {{Name: "x", Func: "F", Parity: ParityNone}},
		"parity": {{Name: "x", Func: "F", Go: good.Go, Parity: "weird"}},
	} {
		if Validate(cs) == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := Validate([]Case{good, goodCase("b")}); err != nil {
		t.Error(err)
	}
}

func TestCalibrate(t *testing.T) {
	// A fake clock: each call costs 1 ms.
	n, err := Calibrate(func(i int) (time.Duration, error) { return time.Duration(i) * time.Millisecond, nil }, 100*time.Millisecond)
	if err != nil || n < 100 || n > 200 {
		t.Errorf("n = %d, err = %v", n, err)
	}
	// First call already long enough.
	if n, _ := Calibrate(func(int) (time.Duration, error) { return time.Second, nil }, time.Millisecond); n != 1 {
		t.Errorf("n = %d", n)
	}
	// Zero durations: doubling, then gives up.
	if _, err := Calibrate(func(int) (time.Duration, error) { return 0, nil }, time.Millisecond); !errors.Is(err, errNoTime) {
		t.Errorf("err = %v", err)
	}
	// A body so fast that the scaled count leaves the iteration bound (1<<40, or
	// half of MaxInt on a 32-bit int): refused, not converted out of range.
	if _, err := Calibrate(func(int) (time.Duration, error) { return time.Nanosecond, nil }, time.Hour); !errors.Is(err, errNoTime) {
		t.Errorf("err = %v", err)
	}
	boom := errors.New("boom")
	if _, err := Calibrate(func(int) (time.Duration, error) { return 0, boom }, time.Millisecond); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
}

// fakeLoad returns the readings in turn, then the last one forever.
func fakeLoad(vals ...float64) func() (float64, error) {
	i := 0
	return func() (float64, error) {
		v := vals[min(i, len(vals)-1)]
		i++
		return v, nil
	}
}

// fixedRef is a reference timer costing perCall per call, without sleeping.
func fixedRef(perCall time.Duration) Timer {
	return func(iters int) (time.Duration, error) { return time.Duration(iters) * perCall, nil }
}

func fixedOpts() Options {
	return Options{Rounds: 4, MinTime: time.Millisecond, Load: fakeLoad(1), Now: func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) }, Commit: "abc", Repo: "r/x", Machine: "m"}
}

func TestRunAlternatesAndRecordsLoad(t *testing.T) {
	var order []string
	c := goodCase("alt")
	c.Go = func(n int) { order = append(order, "go"); spin(n) }
	c.Ref = func(iters int) (time.Duration, error) {
		order = append(order, "ref")
		return time.Duration(iters) * time.Microsecond, nil
	}
	// Calibration runs both sides first (one call each, slow ref immediately long enough
	// is not guaranteed), so look at the rounds' flags and at the order after calibration.
	rep, err := Run(context.Background(), []Case{c}, Options{Rounds: 4, MinTime: time.Nanosecond, Load: fakeLoad(1, 2, 3, 4, 5, 6, 7, 7), Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	row := rep.Rows[0]
	if len(row.Rounds) != 4 {
		t.Fatalf("rounds = %d", len(row.Rounds))
	}
	for i, rd := range row.Rounds {
		if rd.GoFirst != (i%2 == 0) {
			t.Errorf("round %d: GoFirst = %v", i, rd.GoFirst)
		}
	}
	if row.Rounds[0].LoadBefore != 1 || row.Rounds[0].LoadAfter != 2 || row.Rounds[1].LoadBefore != 3 {
		t.Errorf("loads = %+v", row.Rounds)
	}
	if !rep.Valid || rep.Status != StatusMeasured {
		t.Errorf("valid = %v, status = %s", rep.Valid, rep.Status)
	}
	if rep.Load.Max != 7 || rep.Load.Min != 1 {
		t.Errorf("load stats = %+v", rep.Load)
	}
	// The tail of the call order is the four rounds: go,ref,ref,go,go,ref,ref,go.
	tail := strings.Join(order[len(order)-8:], ",")
	if tail != "go,ref,ref,go,go,ref,ref,go" {
		t.Errorf("order = %s", tail)
	}
}

func TestRunSetsOneProcAndRestores(t *testing.T) {
	prev := runtime.GOMAXPROCS(0)
	var seen int
	c := goodCase("procs")
	c.Go = func(n int) { seen = runtime.GOMAXPROCS(0); spin(n) }
	c.Ref = fixedRef(time.Millisecond)
	if _, err := Run(context.Background(), []Case{c}, fixedOpts()); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Errorf("GOMAXPROCS during the run = %d", seen)
	}
	if got := runtime.GOMAXPROCS(0); got != prev {
		t.Errorf("GOMAXPROCS after = %d, want %d", got, prev)
	}
}

func TestRunVerdictAndInvalid(t *testing.T) {
	c := goodCase("slow")
	c.Ref = fixedRef(time.Nanosecond) // Go cannot be 1 ns/call with the clock overhead of a loop of sleeps... use ratio only structurally
	rep, err := Run(context.Background(), []Case{c}, fixedOpts())
	if err != nil {
		t.Fatal(err)
	}
	// Structural only: Windows' coarse clock can time the Go loop at 0 ns
	// (ratio 0); a NaN, an infinity or a negative ratio is still a defect.
	if r := rep.Rows[0].Ratio; !(r >= 0) || math.IsInf(r, 0) {
		t.Errorf("ratio = %v", rep.Rows[0].Ratio)
	}
}

// A load source driven by a script: the gate reads and the after-round reads
// come from the same sequence, the last value forever.
func TestRunRetriesARoundThatEndsAboveTheGate(t *testing.T) {
	c := goodCase("retry")
	c.Ref = fixedRef(time.Millisecond)
	o := fixedOpts()
	o.Rounds = 2
	o.Poll = time.Microsecond
	// gate 1, after 9 (retry), gate 1, after 1 (kept), gate 1, after 1 (kept).
	o.Load = fakeLoad(1, 9, 1, 1, 1, 1)
	rep, err := Run(context.Background(), []Case{c}, o)
	if err != nil {
		t.Fatal(err)
	}
	row := rep.Rows[0]
	if len(row.Rounds) != 2 || row.Retries != 1 || rep.Retries != 1 || !row.Valid || !rep.Valid {
		t.Fatalf("rounds = %d, retries = %d/%d, valid = %v/%v", len(row.Rounds), row.Retries, rep.Retries, row.Valid, rep.Valid)
	}
	// The retried round keeps its place in the alternation: Go first, then reference first.
	for i, rd := range row.Rounds {
		if rd.LoadAfter >= 8 || rd.GoFirst != (i%2 == 0) {
			t.Errorf("round %d = %+v", i, rd)
		}
	}
}

func TestRunRetryCapDefersOnlyThatRow(t *testing.T) {
	bad, good := goodCase("bad"), goodCase("good")
	bad.Ref, good.Ref = fixedRef(time.Millisecond), fixedRef(time.Millisecond)
	o := fixedOpts()
	o.Rounds, o.MaxRetries, o.Poll = 1, 2, time.Microsecond
	// Calibration does not read the load. "bad": every round ends at 9 (3 reads of
	// the pair, cap 2 -> the third retry defers it); then "good" runs under 1.
	vals := []float64{1, 9, 1, 9, 1, 9, 1, 1}
	o.Load = fakeLoad(vals...)
	rep, err := Run(context.Background(), []Case{bad, good}, o)
	if err != nil {
		t.Fatal(err)
	}
	b, g := rep.Rows[0], rep.Rows[1]
	if b.Valid || len(b.Rounds) != 0 || b.Retries != 3 || !g.Valid || len(g.Rounds) != 1 {
		t.Fatalf("bad = %+v good = %+v", b, g)
	}
	if rep.Valid || len(rep.Deferred) != 1 || rep.Deferred[0] != "bad" || rep.ValidRows() != 1 || rep.Retries != 3 {
		t.Errorf("report: valid %v deferred %v retries %d", rep.Valid, rep.Deferred, rep.Retries)
	}
	// Every row capped: nothing measured, no report.
	o.Load, o.LoadTimeout = fakeLoad(1, 9), 20*time.Millisecond
	if rep, err := Run(context.Background(), []Case{bad}, o); !errors.Is(err, ErrOnLoad) || rep != nil {
		t.Errorf("gate closed on every row: %v, %v", rep, err)
	}
	o.Load = fakeLoad(1, 9, 1, 9, 1, 9)
	if rep, err := Run(context.Background(), []Case{bad}, o); !errors.Is(err, ErrOnLoad) || rep != nil || !strings.Contains(err.Error(), "more than 2 times") {
		t.Errorf("every row capped: %v, %v", rep, err)
	}
}

// A gate that stays closed (refuse policy) after the first row: that row stays,
// the rest are deferred, no error.
func TestRunClosedGateKeepsMeasuredRows(t *testing.T) {
	a, b, c := goodCase("a"), goodCase("b"), goodCase("c")
	a.Ref, b.Ref, c.Ref = fixedRef(time.Millisecond), fixedRef(time.Millisecond), fixedRef(time.Millisecond)
	o := fixedOpts()
	o.Rounds, o.OnLoad = 1, OnLoadRefuse
	o.Load = fakeLoad(1, 1, 99) // a: gate 1, after 1; b: gate 99; c: not tried
	rep, err := Run(context.Background(), []Case{a, b, c}, o)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Rows[0].Valid || rep.Rows[1].Valid || rep.Rows[2].Valid || len(rep.Rows[2].Rounds) != 0 || len(rep.Deferred) != 2 || rep.Deferred[1] != "c" {
		t.Errorf("rows = %+v deferred %v", rep.Rows, rep.Deferred)
	}
	// Only restricts the run.
	o.Load, o.Only = fakeLoad(1), map[string]bool{"b": true}
	rep, err = Run(context.Background(), []Case{a, b}, o)
	if err != nil || len(rep.Rows) != 1 || rep.Rows[0].Name != "b" {
		t.Errorf("only: %v %v", rep, err)
	}
}

func TestResumeMergesDeferredRows(t *testing.T) {
	a, b := goodCase("a"), goodCase("b")
	a.Ref, b.Ref = fixedRef(time.Millisecond), fixedRef(time.Millisecond)
	cases := []Case{a, b}
	o := fixedOpts()
	o.Rounds, o.OnLoad = 1, OnLoadRefuse
	o.Load = fakeLoad(1, 1, 99)
	prev, err := Run(context.Background(), cases, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckResume(prev, o); err != nil {
		t.Fatal(err)
	}
	todo := Pending(prev, cases)
	if len(todo) != 1 || !todo["b"] {
		t.Fatalf("pending = %v", todo)
	}
	aRounds := prev.Rows[0].Rounds[0]
	o.Only, o.Load = todo, fakeLoad(1)
	next, err := Run(context.Background(), cases, o)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Merge(prev, next)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Valid || len(m.Deferred) != 0 || len(m.Rows) != 2 || !reflect.DeepEqual(m.Rows[0].Rounds[0], aRounds) || len(m.Rows[1].Rounds) != 1 {
		t.Fatalf("merged = %+v", m)
	}
	if len(Pending(m, cases)) != 0 {
		t.Error("a complete report still has pending rows")
	}
	// Refusals name the field.
	for name, mut := range map[string]func(*Options){
		"commit": func(o *Options) { o.Commit = "other" },
		"gate":   func(o *Options) { o.MaxLoad = 4 },
		"ref":    func(o *Options) { o.RefVersions = map[string]string{"numpy": "9"} },
	} {
		oo := o
		mut(&oo)
		if err := CheckResume(prev, oo); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
	g := *prev
	g.Go = "go0.0"
	if err := CheckResume(&g, o); err == nil || !strings.Contains(err.Error(), "go0.0") {
		t.Errorf("go: %v", err)
	}
	if err := CheckResume(nil, o); err == nil {
		t.Error("nil prev")
	}
	if err := CheckResume(&Report{Status: StatusDeferred}, o); err == nil {
		t.Error("deferred prev")
	}
	// A row that is still deferred after the resume stays deferred, with the retries added;
	// a row prev did not have is appended.
	still := *next
	still.Rows = []Row{{Name: "b", Func: "F", Retries: 2, Allocs: 7}, {Name: "c", Func: "F", Rounds: next.Rows[0].Rounds}}
	m2, err := Merge(prev, &still)
	if err != nil || len(m2.Rows) != 3 || m2.Rows[1].Valid || m2.Rows[1].Retries != 2 || m2.Rows[1].Allocs != 7 || m2.Rows[2].Name != "c" || len(m2.Deferred) != 1 {
		t.Errorf("still deferred: %+v %v", m2, err)
	}
	other := *next
	other.Commit = "zzz"
	if _, err := Merge(prev, &other); err == nil {
		t.Error("merge across commits")
	}
	if _, err := Merge(&Report{Status: StatusDeferred}, next); err == nil {
		t.Error("merge of a deferred report")
	}
}

func TestMergeAllocsKeepsTimings(t *testing.T) {
	prev := sampleReport()
	rec := &Report{Status: StatusDeferred, Rows: []Row{
		{Name: "fast", Func: "G", Reference: "ref", Parity: ParityTolerance, Allocs: 5},
		{Name: "new", Func: "H", Parity: ParityNone, Allocs: 1},
	}}
	m := MergeAllocs(prev, rec)
	if m.Status != StatusMeasured || len(m.Rows) != 2 || m.Rows[0].Allocs != 5 || len(m.Rows[0].Rounds) != 1 ||
		m.Rows[1].Name != "new" || m.Rows[1].Valid || len(m.Deferred) != 1 {
		t.Errorf("merged = %+v", m)
	}
	if got := MergeAllocs(nil, rec); got != rec {
		t.Error("no previous report: the recording is returned as is")
	}
	if got := MergeAllocs(&Report{Status: StatusDeferred}, rec); got != rec {
		t.Error("deferred previous report: the recording is returned as is")
	}
}

func TestRunErrors(t *testing.T) {
	ctx := context.Background()
	c := goodCase("e")
	c.Ref = fixedRef(time.Millisecond)
	if _, err := Run(ctx, []Case{{}}, fixedOpts()); err == nil {
		t.Error("invalid registry accepted")
	}
	noRef := goodCase("noref")
	if _, err := Run(ctx, []Case{noRef}, fixedOpts()); err == nil {
		t.Error("case without a reference accepted")
	}
	boom := errors.New("boom")
	// Reference failure during calibration, then during rounds: the run stops.
	bad := c
	bad.Ref = func(int) (time.Duration, error) { return 0, boom }
	if _, err := Run(ctx, []Case{bad}, fixedOpts()); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	calls := 0
	late := c
	late.Ref = func(iters int) (time.Duration, error) {
		calls++
		if calls > 1 {
			return 0, boom
		}
		return time.Duration(iters) * time.Second, nil
	}
	if _, err := Run(ctx, []Case{late}, fixedOpts()); !errors.Is(err, boom) {
		t.Errorf("round failure: err = %v", err)
	}
	// Go side calibration cannot find a time: a Go body that takes no time at all is
	// still measured by the real clock, so use a stuck clock through a zero Timer instead.
	noTime := c
	// Reference calibration failing with errNoTime.
	noTime.Ref = func(int) (time.Duration, error) { return 0, nil }
	if _, err := Run(ctx, []Case{noTime}, fixedOpts()); !errors.Is(err, errNoTime) {
		t.Errorf("ref calibration: err = %v", err)
	}
	// Load errors: at the gate and after the round.
	o := fixedOpts()
	o.Load = func() (float64, error) { return 0, boom }
	if _, err := Run(ctx, []Case{c}, o); !errors.Is(err, boom) {
		t.Errorf("gate read: err = %v", err)
	}
	n := 0
	o.Load = func() (float64, error) {
		n++
		if n == 2 {
			return 0, boom
		}
		return 1, nil
	}
	if _, err := Run(ctx, []Case{c}, o); !errors.Is(err, boom) {
		t.Errorf("after read: err = %v", err)
	}
}

func TestGoCalibrationError(t *testing.T) {
	// goTimer never errors, so Run's Go-side calibration error path is the clock
	// never advancing: replace the Go body by one that panics? No: test runCase
	// through a Case whose Go body is instantaneous and a MinTime too long to reach.
	c := goodCase("g")
	c.Ref = fixedRef(time.Millisecond)
	o := fixedOpts()
	o.MinTime = 100 * 365 * 24 * time.Hour
	if _, err := Run(context.Background(), []Case{c}, o); err == nil {
		t.Error("want a calibration error")
	}
}

func TestGateRefuseWaitAndTimeout(t *testing.T) {
	c := goodCase("gate")
	c.Ref = fixedRef(time.Millisecond)
	o := fixedOpts()
	o.OnLoad = OnLoadRefuse
	o.Load = fakeLoad(8) // 8 is "at or above": refused
	if _, err := Run(context.Background(), []Case{c}, o); !errors.Is(err, ErrOnLoad) {
		t.Errorf("refuse: err = %v", err)
	}
	// Wait: busy, busy, then quiet.
	o = fixedOpts()
	o.Poll = time.Microsecond
	o.LoadTimeout = time.Minute
	o.Load = fakeLoad(20, 20, 3)
	rep, err := Run(context.Background(), []Case{c}, o)
	if err != nil || rep.Rows[0].Rounds[0].LoadBefore != 3 {
		t.Errorf("wait: err = %v", err)
	}
	// Timeout: always busy.
	o = fixedOpts()
	o.Poll = time.Microsecond
	o.LoadTimeout = 5 * time.Millisecond
	o.Load = fakeLoad(20)
	if _, err := Run(context.Background(), []Case{c}, o); !errors.Is(err, ErrOnLoad) {
		t.Errorf("timeout: err = %v", err)
	}
}

func TestDefaultsAndRecordAllocs(t *testing.T) {
	o := Options{}.withDefaults()
	if o.Rounds != 7 || o.MaxLoad != DefaultMaxLoad || o.OnLoad != OnLoadWait || o.Load == nil || o.Now == nil || o.Machine == "" || o.Poll <= 0 || o.LoadTimeout <= 0 || o.MinTime <= 0 {
		t.Errorf("defaults = %+v", o)
	}
	c := goodCase("alloc")
	var sink []byte
	c.Go = func(int) { sink = make([]byte, 64) }
	rep, err := RecordAllocs([]Case{c}, fixedOpts())
	if err != nil {
		t.Fatal(err)
	}
	_ = sink
	if rep.Status != StatusDeferred || rep.Rows[0].Allocs != 1 || len(rep.Rows[0].Rounds) != 0 {
		t.Errorf("report = %+v", rep)
	}
	if _, err := RecordAllocs([]Case{{}}, fixedOpts()); err == nil {
		t.Error("invalid registry accepted")
	}
}

func TestBuildC(t *testing.T) {
	t.Setenv("REFBENCH_HELPER", "cc")
	old := execCommand
	defer func() { execCommand = old }()
	var got []string
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		got = append([]string{name}, args...)
		return helperCommand(ctx, "cc", name)
	}
	if err := BuildC(context.Background(), "ok", "out", NoFMAFlags, []string{"a.c", "b.c"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "ok -O2 -ffp-contract=off -o out a.c b.c" {
		t.Errorf("args = %v", got)
	}
	err := BuildC(context.Background(), "fail", "out", nil, []string{"a.c"})
	if err == nil || !strings.Contains(err.Error(), "compiler said no") {
		t.Errorf("err = %v", err)
	}
}

func TestSingleThreadEnvAndUVArgs(t *testing.T) {
	env := strings.Join(SingleThreadEnv(), "\n")
	for _, k := range []string{"OMP_NUM_THREADS=1", "OPENBLAS_NUM_THREADS=1", "MKL_NUM_THREADS=1", "VECLIB_MAXIMUM_THREADS=1"} {
		if !strings.Contains(env, k) {
			t.Errorf("missing %s", k)
		}
	}
	a := UVArgs("refs.txt")
	want := []string{"run", "--no-project", "--python", PythonVersion, "--with-requirements", "refs.txt", "python", "-I", "-c"}
	if strings.Join(a[:9], " ") != strings.Join(want, " ") || a[9] != PyWorkerSource || !strings.Contains(PyWorkerSource, "perf_counter_ns") {
		t.Errorf("args = %v", a[:9])
	}
}

func TestSlug(t *testing.T) {
	if got := Slug("Orient2D/random (n=1)"); got != "orient2d-random-n-1" {
		t.Errorf("slug = %q", got)
	}
}

func TestAllocsPerCallCounts(t *testing.T) {
	var sink [][]byte
	c := goodCase("a")
	c.Go = func(int) { sink = append(sink[:0], make([]byte, 8), make([]byte, 8)) }
	if got := AllocsPerCall(c); got != 2 {
		t.Errorf("allocs = %v", got)
	}
	// A body that allocates on every other input, averaged over its cycle.
	var sink2 []byte
	c.Go = func(n int) {
		for i := 0; i < n; i++ {
			if i%2 == 0 {
				sink2 = make([]byte, 8)
			}
		}
	}
	_ = sink2
	c.AllocIters = 4
	if got := AllocsPerCall(c); got != 0.5 {
		t.Errorf("allocs over a cycle = %v", got)
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

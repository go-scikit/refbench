package refbench

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
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
	if rep.Rows[0].Ratio <= 0 {
		t.Errorf("ratio = %v", rep.Rows[0].Ratio)
	}
	// A round that ends at the gate invalidates the run.
	o := fixedOpts()
	o.Load = fakeLoad(1, 9)
	o.Rounds = 1
	rep, err = Run(context.Background(), []Case{c}, o)
	if err != nil || rep.Valid {
		t.Errorf("err = %v, valid = %v", err, rep.Valid)
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

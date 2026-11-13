package refbench

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFasterButNaNOnTenPercentIsNotAhead(t *testing.T) {
	// The reviewer's probe: Go has a tiny finite error but is NaN on 10% of the
	// points where the reference returns numbers; "ahead" would promise "at
	// least as accurate".
	goAcc := &AccResult{MaxULP: ULP(math.Inf(1)), MaxULPFin: 0.5, Coverage: 0.9, Disagree: 1, Points: 10}
	ref := &AccResult{MaxULP: 1, MaxULPFin: 1, Coverage: 1, Points: 10}
	if got := Label(Faster, goAcc, ref, 0); got != LabelFasterLessAccurate {
		t.Errorf("Label = %q, want %q", got, LabelFasterLessAccurate)
	}
	if got := Label(Tie, goAcc, ref, 0); got != LabelFasterLessAccurate {
		t.Errorf("tie: Label = %q", got)
	}
}

func TestPrintableASCIIBoundaries(t *testing.T) {
	for c, want := range map[byte]bool{0x00: false, 0x1f: false, 0x20: true, 0x41: true, 0x7e: true, 0x7f: false, 0x80: false, 0xff: false} {
		if got := printableASCII([]byte{'a', c, 'b'}); got != want {
			t.Errorf("byte %#x: printable = %v, want %v", c, got, want)
		}
	}
	if !printableASCII(nil) {
		t.Error("empty is printable")
	}
	// Through the parser: space (0x20) is the separator, '~' (0x7e) is allowed.
	if r, err := ParseRBW1Reply([]byte("ok ~ a~")); err != nil || len(r.Args) != 2 {
		t.Errorf("0x7e: %v %v", r, err)
	}
	if _, err := ParseRBW1Reply([]byte("ok \x7f")); err == nil {
		t.Error("0x7f accepted")
	}
	if _, err := ParseRBW1Reply([]byte("ok \x1f")); err == nil {
		t.Error("0x1f accepted")
	}
}

func TestVersionPairsBoundary(t *testing.T) {
	pairs := func(n int) []byte {
		var b strings.Builder
		b.WriteString("v")
		for i := 0; i < n; i++ {
			b.WriteString(" k" + strconv.Itoa(i) + "=1")
		}
		return []byte(b.String())
	}
	if r, err := ParseRBW1Reply(pairs(rbw1MaxVersions)); err != nil || len(r.Versions) != rbw1MaxVersions {
		t.Errorf("256 pairs: %v", err)
	}
	if _, err := ParseRBW1Reply(pairs(rbw1MaxVersions + 1)); err == nil {
		t.Error("257 pairs accepted")
	}
	if r, err := ParseRBW1Reply(pairs(1)); err != nil || len(r.Versions) != 1 {
		t.Errorf("1 pair: %v", err)
	}
	if _, err := ParseRBW1Reply([]byte("v")); err == nil {
		t.Error("no pairs accepted")
	}
}

func TestTimeBoundary(t *testing.T) {
	dir := t.TempDir()
	reply := ""
	w := fake(t, dir, []string{"f"}, func(string) string { return reply })
	// 2^63 ns does not fit a time.Duration; the largest float below it does.
	reply = "ns 9223372036854775808"
	if _, err := w.Time("x", "f", 1); err == nil || !strings.Contains(err.Error(), "overflows") {
		t.Errorf("2^63: %v", err)
	}
	reply = "ns 9223372036854774784"
	if d, err := w.Time("x", "f", 1); err != nil || d <= 0 {
		t.Errorf("2^63 - 1024: %v %v", d, err)
	}
	reply = "ns 0"
	if d, err := w.Time("x", "f", 1); err != nil || d != 0 {
		t.Errorf("0: %v %v", d, err)
	}
}

func TestEvalReplyBoundaries(t *testing.T) {
	dir := t.TempDir()
	reply := ""
	w := fake(t, dir, []string{"f"}, func(string) string { return reply })
	out := filepath.Join(dir, "o")
	for _, c := range []struct {
		reply string
		a, b  int
		ok    bool
	}{
		{"ok 0 0", 0, 0, true}, {"ok 1 0", 1, 0, true}, {"ok 0 1", 0, 1, true}, {"ok 7 9", 7, 9, true},
		{"ok -1 0", 0, 0, false}, {"ok 0 -1", 0, 0, false}, {"ok 1", 0, 0, false}, {"ok 1 2 3", 0, 0, false},
	} {
		reply = c.reply
		a, b, err := w.Eval("x", "f", out)
		if (err == nil) != c.ok || (c.ok && (a != c.a || b != c.b)) {
			t.Errorf("%q: %d %d %v", c.reply, a, b, err)
		}
	}
	// The worker's own error is what comes back, not a generic one.
	reply = "err domain error at point 3"
	if _, _, err := w.Eval("x", "f", out); err == nil || !strings.Contains(err.Error(), "domain error at point 3") {
		t.Errorf("worker error lost: %v", err)
	}
	// The out path is refused on the Go side before anything is sent.
	sent := 0
	w2 := fake(t, dir, []string{"f"}, func(string) string { sent++; return "ok 0 0" })
	for _, p := range []string{"relative", filepath.Join(filepath.Dir(dir), "o"), dir, dir + string(filepath.Separator) + ".." + string(filepath.Separator) + "o"} {
		if _, _, err := w2.Eval("x", "f", p); err == nil || !strings.Contains(err.Error(), "inside") {
			t.Errorf("out path %q: %v", p, err)
		}
	}
	if sent != 0 {
		t.Errorf("%d requests reached the worker", sent)
	}
}

func TestLoadDimensionBoundaries(t *testing.T) {
	dir := t.TempDir()
	var last string
	w := fake(t, dir, []string{"f"}, func(l string) string { last = l; return "ok " + strings.Repeat("0", 64) })
	zero, p := strings.Repeat("0", 64), filepath.Join(dir, "a")
	const maxRows = RBW1MaxData / 8
	for _, c := range []struct {
		cols, rows int
		ok         bool
	}{
		{1, 1, true}, {0, 1, false}, {1, 0, false}, {0, 0, false}, {-1, 1, false}, {1, -1, false},
		{1, maxRows, true}, {1, maxRows + 1, false},
		{2, maxRows / 2, true}, {2, maxRows/2 + 1, false},
		{3, maxRows / 3, true}, {3, maxRows/3 + 1, false},
		{maxRows, 1, true}, {maxRows + 1, 1, false},
		{math.MaxInt32, math.MaxInt32, false}, {math.MaxInt32, 1, false},
	} {
		last = ""
		err := w.Load("x", p, c.cols, c.rows, zero)
		if (err == nil) != c.ok || (c.ok && last == "") || (!c.ok && last != "") {
			t.Errorf("%d x %d: err %v, sent %q", c.cols, c.rows, err, last)
		}
	}
}

func TestWriteF64NameAndLimit(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "run")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape", "a/b", "", "a b", "a.b", strings.Repeat("a", 65)} {
		_, _, _, err := WriteF64(dir, name, []float64{1})
		if err == nil || !strings.Contains(err.Error(), "plain identifier") {
			t.Errorf("name %q: %v", name, err)
		}
	}
	for _, f := range []string{filepath.Join(parent, "escape.f64"), filepath.Join(dir, "a", "b.f64")} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s was written", f)
		}
	}
	if _, _, _, err := WriteF64(dir, strings.Repeat("a", 64), []float64{1}); err != nil {
		t.Errorf("a 64-character name: %v", err)
	}
	old := dataLimit
	defer func() { dataLimit = old }()
	dataLimit = 16 // two float64
	if _, _, _, err := WriteF64(dir, "two", []float64{1, 2}); err != nil {
		t.Errorf("exactly at the limit: %v", err)
	}
	if _, _, _, err := WriteF64(dir, "three", []float64{1, 2, 3}); err == nil {
		t.Error("one value over the limit accepted")
	}
	if _, _, _, err := WriteF64(dir, "wide", []float64{1}, []float64{2}); err != nil {
		t.Errorf("two columns of one row at the limit: %v", err)
	}
	if _, _, _, err := WriteF64(dir, "wide3", []float64{1}, []float64{2}, []float64{3}); err == nil {
		t.Error("three columns over the limit accepted")
	}
	// ReadF64 bounds.
	empty := filepath.Join(dir, "empty.f64")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadF64(empty, 0); err != nil || len(got) != 0 {
		t.Errorf("n = 0: %v %v", got, err)
	}
	two := filepath.Join(dir, "two.f64")
	if got, err := ReadF64(two, 2); err != nil || len(got) != 2 {
		t.Errorf("n at the limit: %v %v", got, err)
	}
	if _, err := ReadF64(two, 3); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("n over the limit: %v", err)
	}
	if _, err := ReadF64(two, -1); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("n = -1: %v", err)
	}
}

func TestValidateBudgetBoundary(t *testing.T) {
	score := func([]float64) AccResult { return AccResult{} }
	eval := func() ([]float64, error) { return nil, nil }
	mk := func(b float64) Case {
		return Case{Name: "a", Func: "A", Parity: ParityNone, Go: func(int) {}, Accuracy: &Accuracy{Go: eval, Score: score, Budget: b}}
	}
	for b, ok := range map[float64]bool{0: true, 1e-300: true, 4: true, -1e-300: false, -1: false, math.NaN(): false} {
		if err := Validate([]Case{mk(b)}); (err == nil) != ok {
			t.Errorf("Budget %v: %v", b, err)
		}
	}
}

func TestRunWithNothingToMeasureIsNotAnError(t *testing.T) {
	var log []string
	c := multiCase(&log)
	rep, err := Run(context.Background(), []Case{c}, Options{Only: map[string]bool{"nothing": true}, Load: func() (float64, error) { return 1, nil }})
	if err != nil || len(rep.Rows) != 0 {
		t.Errorf("an empty resume: %v %+v", err, rep)
	}
	// And a gate that never opens, with rows to measure, is an error.
	_, err = Run(context.Background(), []Case{c}, Options{OnLoad: OnLoadRefuse, Load: func() (float64, error) { return 99, nil }})
	if err == nil {
		t.Error("a closed gate with a row to measure gave a report")
	}
}

func TestCloseWaitsForTheProcessAndReportsItsExit(t *testing.T) {
	helperRBW1(t, "exit3")
	w, err := StartRBW1(context.Background(), RBW1Config{Dir: t.TempDir(), Funcs: []string{"f"}}, os.Environ(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("Close = %v, want the process's exit status", err)
	}
	// A worker with no process (a pair of streams) just closes its input.
	if err := fake(t, t.TempDir(), []string{"f"}, func(string) string { return "ok" }).Close(); err != nil {
		t.Errorf("Close of a stream worker: %v", err)
	}
}

func TestRefRowsAndLabels(t *testing.T) {
	if rows := (Case{}).refRows(); rows != nil {
		t.Errorf("a case with no Refs has refRows = %#v, want nil", rows)
	}
	if got := (RefRow{ID: "a:b", Name: "Nice"}).label(); got != "Nice" {
		t.Errorf("label = %q", got)
	}
	if got := (RefRow{ID: "a:b"}).label(); got != "a:b" {
		t.Errorf("label = %q", got)
	}
}

func TestNilAccuracyCellsAndScoring(t *testing.T) {
	if ulpCell(nil) != "—" || covCell(nil) != "—" || accCell(nil) != "no accuracy" {
		t.Errorf("nil cells: %q %q %q", ulpCell(nil), covCell(nil), accCell(nil))
	}
	// No Accuracy: nothing is scored, nothing recorded.
	var log []string
	c := multiCase(&log)
	c.Go = spinFor(12000)
	c.Accuracy, c.RefEval = nil, nil
	row := c.row(0)
	if err := scoreAccuracy(c, &row); err != nil || row.GoAccuracy != nil || row.PrimaryAccuracy != nil || row.GoBudget != 0 || row.Refs[0].Accuracy != nil {
		t.Errorf("without Accuracy: %v %+v", err, row)
	}
	// Accuracy, but a side without Eval: that side has none, the others do.
	c = multiCase(&log)
	c.RefEval = nil
	c.Refs[0].Eval = nil
	row = c.row(0)
	if err := scoreAccuracy(c, &row); err != nil {
		t.Fatal(err)
	}
	if row.GoAccuracy == nil || row.PrimaryAccuracy != nil || row.Refs[0].Accuracy != nil || row.GoBudget != 1 {
		t.Errorf("a side without Eval: %+v", row)
	}
}

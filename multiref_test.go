package refbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fixedTimer returns a Timer of exactly ns nanoseconds per call.
func fixedTimer(ns int64, log *[]string, id string) Timer {
	return func(iters int) (time.Duration, error) {
		return time.Duration(ns * int64(iters)), nil
	}
}

func multiCase(log *[]string) Case {
	return Case{
		Name: "gamma", Func: "Gamma", Reference: "scipy.special.gamma", Parity: ParityNone,
		Go:  spinFor(12000),
		Ref: fixedTimer(10000, log, SidePrimary),
		Refs: []Ref{
			{ID: "julia:Bessels.gamma", Name: "Bessels.gamma", License: "MIT", Timer: fixedTimer(5000, log, "julia:Bessels.gamma"),
				Eval: func() ([]float64, error) { return []float64{1, 3}, nil }},
			{ID: "boost:tgamma", License: "BSL-1.0", Timer: fixedTimer(20000, log, "boost:tgamma")},
		},
		RefEval: func() ([]float64, error) { return []float64{1, 1}, nil },
		Accuracy: &Accuracy{
			Go:     func() ([]float64, error) { return []float64{1, 1}, nil },
			Budget: 1,
			Score: func(got []float64) AccResult {
				return AccResult{MaxULP: ULP(got[1] - 1), MaxULPFin: ULP(got[1] - 1), Coverage: 1, Points: len(got)}
			},
		},
	}
}

func runMulti(t *testing.T, rounds int) (*Report, []string) {
	t.Helper()
	var log []string
	c := multiCase(&log)
	opt := Options{Rounds: rounds, MinTime: time.Millisecond, Load: func() (float64, error) { return 1, nil }, Poll: time.Millisecond}
	rep, err := Run(context.Background(), []Case{c}, opt)
	if err != nil {
		t.Fatal(err)
	}
	return rep, log
}

func TestRunRotatesSidesAndWritesSchema2(t *testing.T) {
	rep, _ := runMulti(t, 4)
	if rep.Schema != SchemaMultiRef {
		t.Fatalf("schema = %d", rep.Schema)
	}
	row := rep.Rows[0]
	if len(row.Rounds) != 4 { // 4 sides: 4 rounds are already a multiple
		t.Fatalf("rounds = %d", len(row.Rounds))
	}
	// Every side takes every position once per k rounds.
	pos := map[string]map[int]int{}
	for _, rd := range row.Rounds {
		if len(rd.Order) != 4 {
			t.Fatalf("order = %v", rd.Order)
		}
		for p, s := range rd.Order {
			if pos[s] == nil {
				pos[s] = map[int]int{}
			}
			pos[s][p]++
		}
	}
	for s, m := range pos {
		for p := 0; p < 4; p++ {
			if m[p] != 1 {
				t.Errorf("side %s at position %d: %d times", s, p, m[p])
			}
		}
	}
	// The best is the fastest side, the verdict against it.
	if row.Best != "julia:Bessels.gamma" || row.BestVerdict != Slower {
		t.Errorf("best = %q %q", row.Best, row.BestVerdict)
	}
}

func TestRoundsRoundedUpToTheNumberOfSides(t *testing.T) {
	rep, _ := runMulti(t, 5)
	if n := len(rep.Rows[0].Rounds); n != 8 {
		t.Errorf("rounds = %d, want 8", n)
	}
}

func TestTwoSidesStillAlternate(t *testing.T) {
	var log []string
	c := multiCase(&log)
	c.Refs, c.Accuracy, c.RefEval = nil, nil, nil
	rep, err := Run(context.Background(), []Case{c}, Options{Rounds: 3, MinTime: time.Millisecond, Load: func() (float64, error) { return 1, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Schema != SchemaVersion || len(rep.Rows[0].Rounds) != 3 {
		t.Fatalf("schema %d, %d rounds", rep.Schema, len(rep.Rows[0].Rounds))
	}
	for i, rd := range rep.Rows[0].Rounds {
		if rd.GoFirst != (i%2 == 0) || rd.Order != nil || rd.RefsNs != nil {
			t.Errorf("round %d = %+v", i, rd)
		}
	}
}

func TestPerReferenceVerdictsLabelsAndAccuracy(t *testing.T) {
	rep, _ := runMulti(t, 4)
	row := rep.Rows[0]
	bessels, boost := row.Refs[0], row.Refs[1]
	if bessels.Verdict != Slower || bessels.Ratio <= 1 {
		t.Errorf("bessels = %+v", bessels)
	}
	// Bessels.gamma is faster than Go and less accurate than Go's stated bound
	// (2 ulp against a budget of 1... Go has 0): a tradeoff, still a defect.
	if bessels.Label != LabelTradeoff {
		t.Errorf("label = %q", bessels.Label)
	}
	if boost.Verdict != Faster || boost.Label != "" {
		t.Errorf("boost = %+v (no accuracy: no claim)", boost)
	}
	if row.GoAccuracy == nil || row.PrimaryAccuracy == nil || row.GoBudget != 1 {
		t.Errorf("accuracy not recorded: %+v", row)
	}
	if bessels.Accuracy == nil || bessels.Accuracy.MaxULP != 2 {
		t.Errorf("bessels accuracy = %+v", bessels.Accuracy)
	}
}

func TestLabel(t *testing.T) {
	two := &AccResult{MaxULP: 2}
	one := &AccResult{MaxULP: 1}
	zero := &AccResult{}
	cases := []struct {
		v       string
		g, r    *AccResult
		budget  float64
		want    string
		comment string
	}{
		{Slower, one, one, 0, LabelBehind, "as accurate"},
		{Slower, one, two, 0, LabelTradeoff, "less accurate than Go's own max"},
		{Slower, two, one, 5, LabelBehind, "budget wider than the ref"},
		{Slower, one, two, 1.5, LabelTradeoff, "budget"},
		{Slower, nil, two, 0, LabelBehind, "no Go accuracy"},
		{Faster, one, two, 0, LabelAhead, ""},
		{Tie, one, one, 0, LabelAhead, ""},
		{Faster, two, one, 0, LabelFasterLessAccurate, ""},
		{Faster, zero, nil, 0, "", "no ref accuracy"},
		{"", one, one, 0, "", "no verdict"},
	}
	for _, c := range cases {
		if got := Label(c.v, c.g, c.r, c.budget); got != c.want {
			t.Errorf("Label(%s, %v, %v, %v) = %q, want %q (%s)", c.v, c.g, c.r, c.budget, got, c.want, c.comment)
		}
	}
}

func TestIssueAgainstAnyFasterReference(t *testing.T) {
	rep, _ := runMulti(t, 4)
	// Make the primary slower than Go (Go ties or beats it) and keep the
	// less accurate Julia reference faster: the issue is still drafted.
	issues, err := Issues(rep)
	if err != nil || len(issues) != 1 {
		t.Fatalf("issues = %v, %v", issues, err)
	}
	is := issues[0]
	if !strings.Contains(is.Title, "Bessels.gamma") || !strings.Contains(is.Body, "tradeoff") || !strings.Contains(is.Body, "julia:Bessels.gamma") || !strings.Contains(is.Body, "BSL-1.0") {
		t.Errorf("issue = %+v", is)
	}
	// Only a reference that is not the primary is faster.
	var log []string
	c := multiCase(&log)
	c.Ref = fixedTimer(15000, &log, SidePrimary) // Go beats SciPy, but not Julia
	rep2, err := Run(context.Background(), []Case{c}, Options{Rounds: 4, MinTime: time.Millisecond, Load: func() (float64, error) { return 1, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Rows[0].Verdict != Faster {
		t.Fatalf("setup: primary verdict %q", rep2.Rows[0].Verdict)
	}
	got, _ := Issues(rep2)
	if len(got) != 1 || !strings.Contains(got[0].Title, "Bessels.gamma") {
		t.Errorf("no issue for a row slower than Julia only: %v", got)
	}
}

func TestSchema2RoundTripAndTable(t *testing.T) {
	rep, _ := runMulti(t, 4)
	var b bytes.Buffer
	if err := rep.Write(&b); err != nil {
		t.Fatal(err)
	}
	js := b.String()
	back, err := ReadReport(strings.NewReader(js))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(Table(back), Table(rep)) {
		t.Error("table differs after a round trip")
	}
	tab := string(Table(back))
	for _, want := range []string{"## Against other references", "Bessels.gamma", "boost:tgamma", "tradeoff"} {
		if !strings.Contains(tab, want) {
			t.Errorf("table lacks %q:\n%s", want, tab)
		}
	}
	// Derived fields are never trusted from the file.
	s := strings.Replace(js, `"best": "julia:Bessels.gamma"`, `"best": "boost:tgamma"`, 1)
	back, err = ReadReport(strings.NewReader(s))
	if err != nil || back.Rows[0].Best != "julia:Bessels.gamma" {
		t.Errorf("best trusted from the file: %v", err)
	}
}

func TestTableDeferredAndInvalidRows(t *testing.T) {
	var log []string
	c := multiCase(&log)
	rec, err := RecordAllocs([]Case{c}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Schema != SchemaMultiRef || len(rec.Rows[0].Refs) != 2 {
		t.Fatalf("deferred record: schema %d refs %v", rec.Schema, rec.Rows[0].Refs)
	}
	if tab := string(Table(rec)); !strings.Contains(tab, "deferred") {
		t.Errorf("table = %s", tab)
	}
	rep, _ := runMulti(t, 4)
	rep.Rows[0].Rounds[0].LoadAfter = 99
	if tab := string(Table(rep)); !strings.Contains(tab, "invalid") {
		t.Errorf("table = %s", tab)
	}
	// A row without references in a schema 2 table is skipped by the section.
	rep.Rows = append(rep.Rows, Row{Name: "plain", Func: "P"})
	if tab := string(Table(rep)); strings.Contains(tab, "| plain |") && strings.Contains(tab[strings.Index(tab, "## Against"):], "| plain |") {
		t.Error("plain row in the references section")
	}
}

func TestULPJSON(t *testing.T) {
	b, err := json.Marshal(AccResult{MaxULP: ULP(math.Inf(1)), MaxULPFin: 2.5, Coverage: 0.5, Points: 4})
	if err != nil || !strings.Contains(string(b), `"max_ulp":"inf"`) || !strings.Contains(string(b), `"max_ulp_fin":2.5`) {
		t.Fatalf("%s %v", b, err)
	}
	var a AccResult
	if err := json.Unmarshal(b, &a); err != nil || !math.IsInf(float64(a.MaxULP), 1) || a.MaxULPFin != 2.5 {
		t.Fatalf("%+v %v", a, err)
	}
	for _, bad := range []ULP{ULP(math.NaN()), ULP(math.Inf(-1)), -1} {
		if _, err := json.Marshal(bad); err == nil {
			t.Errorf("%v encoded", bad)
		}
	}
	if err := json.Unmarshal([]byte(`"nan"`), new(ULP)); err == nil {
		t.Error(`"nan" decoded`)
	}
}

func TestValidateRefs(t *testing.T) {
	base := func() Case {
		return Case{Name: "a", Func: "A", Parity: ParityNone, Go: func(int) {}, Refs: []Ref{{ID: "julia:x", License: "MIT"}}}
	}
	if err := Validate([]Case{base()}); err != nil {
		t.Fatal(err)
	}
	mut := map[string]func(*Case){
		"id":      func(c *Case) { c.Refs[0].ID = "nokind" },
		"dup":     func(c *Case) { c.Refs = append(c.Refs, c.Refs[0]) },
		"license": func(c *Case) { c.Refs[0].License = " " },
		"gpl":     func(c *Case) { c.Refs[0].License = "GPL-3.0" },
		"lgpl":    func(c *Case) { c.Refs[0].License = "LGPL-3.0" },
		"acc":     func(c *Case) { c.Accuracy = &Accuracy{} },
		"budget": func(c *Case) {
			c.Accuracy = &Accuracy{Go: func() ([]float64, error) { return nil, nil }, Score: func([]float64) AccResult { return AccResult{} }, Budget: -1}
		},
		"reserved": func(c *Case) { c.Refs[0].ID = "go:primary" }, // valid: kind "go" is just a kind
	}
	for name, f := range mut {
		c := base()
		f(&c)
		err := Validate([]Case{c})
		if name == "reserved" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A GPL reference is fine when it is a black-box kind.
	c := base()
	c.Refs[0] = Ref{ID: "r:besselI", License: "GPL-black-box"}
	if err := Validate([]Case{c}); err != nil {
		t.Errorf("black box refused: %v", err)
	}
	if RefKind("julia:x") != "julia" {
		t.Error("RefKind")
	}
}

func TestRunRefErrors(t *testing.T) {
	var log []string
	c := multiCase(&log)
	c.Refs[0].Timer = nil
	if _, err := Run(context.Background(), []Case{c}, Options{}); err == nil || !strings.Contains(err.Error(), "no timer") {
		t.Errorf("nil ref timer: %v", err)
	}
	boom := errors.New("boom")
	for name, mut := range map[string]func(*Case){
		"ref timer": func(c *Case) { c.Refs[1].Timer = func(int) (time.Duration, error) { return 0, boom } },
		"go eval":   func(c *Case) { c.Accuracy.Go = func() ([]float64, error) { return nil, boom } },
		"ref eval":  func(c *Case) { c.Refs[0].Eval = func() ([]float64, error) { return nil, boom } },
		"primary":   func(c *Case) { c.RefEval = func() ([]float64, error) { return nil, boom } },
		"scorer": func(c *Case) {
			c.Accuracy.Score = func([]float64) AccResult { return AccResult{Coverage: 2} }
		},
	} {
		var l []string
		c := multiCase(&l)
		mut(&c)
		_, err := Run(context.Background(), []Case{c}, Options{Rounds: 2, MinTime: time.Millisecond, Load: func() (float64, error) { return 1, nil }})
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestRetryAndGateDiscardAllSides(t *testing.T) {
	var log []string
	c := multiCase(&log)
	var n atomic.Int32
	load := func() (float64, error) {
		if n.Add(1) == 4 { // the first round ends above the gate once
			return 99, nil
		}
		return 1, nil
	}
	rep, err := Run(context.Background(), []Case{c}, Options{Rounds: 4, MinTime: time.Millisecond, Load: load, OnLoad: OnLoadWait, Poll: time.Millisecond, LoadTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rows[0].Retries != 1 || len(rep.Rows[0].Rounds) != 4 {
		t.Errorf("retries %d rounds %d", rep.Rows[0].Retries, len(rep.Rows[0].Rounds))
	}
	load = func() (float64, error) { return 1, errors.New("load") }
	if _, err := Run(context.Background(), []Case{c}, Options{Rounds: 2, MinTime: time.Millisecond, Load: load}); err == nil {
		t.Error("load error lost")
	}
	// The retry cap defers the row.
	load = func() (float64, error) { return 1, nil }
	always := func() (float64, error) { return 99, nil }
	_ = load
	if _, err := Run(context.Background(), []Case{c}, Options{Rounds: 2, MinTime: time.Millisecond, Load: always, OnLoad: OnLoadRefuse}); !errors.Is(err, ErrOnLoad) {
		t.Errorf("err = %v", err)
	}
}

func TestReadReportRejectsBadSchema2(t *testing.T) {
	rep, _ := runMulti(t, 4)
	var b bytes.Buffer
	if err := rep.Write(&b); err != nil {
		t.Fatal(err)
	}
	good := b.String()
	mut := func(old, new string) string {
		if !strings.Contains(good, old) {
			t.Fatalf("%q not in the file", old)
		}
		return strings.Replace(good, old, new, 1)
	}
	for name, in := range map[string]string{
		"schema1":    mut(`"schema": 2`, `"schema": 1`),
		"badid":      mut(`"id": "boost:tgamma"`, `"id": "boost"`),
		"dupid":      mut(`"id": "boost:tgamma"`, `"id": "julia:Bessels.gamma"`),
		"nolicense":  mut(`"license": "BSL-1.0"`, `"license": ""`),
		"coverage":   mut(`"coverage": 1`, `"coverage": 3`),
		"budget":     mut(`"go_budget": 1`, `"go_budget": -1`),
		"refs_ns":    mut(`"julia:Bessels.gamma": 5000`, `"julia:Bessels.gamma": 0`),
		"order":      mut(`"order": [`, `"order": ["go", `),
		"missing ns": mut(`"boost:tgamma": 20000`, `"x:y": 20`),
	} {
		if _, err := ReadReport(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A round with references on a row with none.
	plain := `{"schema":2,"status":"measured","max_load":8,"rows":[{"name":"a","func":"F","allocs":0,"rounds":[{"load_before":1,"load_after":1,"go_ns":1,"ref_ns":1,"refs_ns":{"x:y":1}}]}]}`
	if _, err := ReadReport(strings.NewReader(plain)); err == nil {
		t.Error("orphan refs_ns accepted")
	}
	// Order naming a side twice.
	dup := strings.Replace(good, `"order": [`, `"order": ["go", "go", "primary", "x:y"], "x": [`, 1)
	if _, err := ReadReport(strings.NewReader(dup)); err == nil {
		t.Error("bad order accepted")
	}
	if bad := (AccResult{Coverage: math.NaN()}); bad.valid() {
		t.Error("NaN coverage valid")
	}
}

func TestCheckRefsAndRunCI(t *testing.T) {
	var log []string
	c := multiCase(&log)
	rec, err := RecordAllocs([]Case{c}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if bad := CheckRefs(rec, []Case{c}); len(bad) != 0 {
		t.Errorf("clean report: %v", bad)
	}
	c2 := c
	c2.Refs = append(append([]Ref(nil), c.Refs...), Ref{ID: "fortran:amos", License: "LicenseRef-public-domain"})
	c2.Refs[0].License = "Apache-2.0"
	c3 := c
	c3.Refs = c.Refs[:1]
	for _, bad := range [][]string{CheckRefs(rec, []Case{c2}), CheckRefs(rec, []Case{c3})} {
		if len(bad) == 0 {
			t.Error("a registry that differs from the report passed")
		}
	}
	if bad := CheckRefs(rec, []Case{{Name: "absent"}}); len(bad) != 0 {
		t.Errorf("absent row: %v", bad)
	}

	// RunCI end to end: no worker, no timing.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n\nfunc Gamma() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var js bytes.Buffer
	if err := rec.Write(&js); err != nil {
		t.Fatal(err)
	}
	cfg := CIConfig{Root: dir, Cases: []Case{c}}
	jp, tp := cfg.Paths()
	if err := os.WriteFile(jp, js.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tp, Table(rec), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunCI(cfg); err != nil {
		t.Fatalf("CI: %v", err)
	}
	cfg.Cases = []Case{c2}
	if err := RunCI(cfg); err == nil || !strings.Contains(err.Error(), "references:") {
		t.Errorf("CI with a changed registry: %v", err)
	}
}

func TestMergeAllocsKeepsOrDefersByReferenceSet(t *testing.T) {
	rep, _ := runMulti(t, 4)
	var log []string
	c := multiCase(&log)
	rec, _ := RecordAllocs([]Case{c}, Options{})
	same := MergeAllocs(rep, rec)
	if len(same.Rows[0].Rounds) != 4 || same.Schema != SchemaMultiRef {
		t.Errorf("same set: %d rounds, schema %d", len(same.Rows[0].Rounds), same.Schema)
	}
	c.Refs = c.Refs[:1]
	c.Refs[0].License = "Apache-2.0"
	rec, _ = RecordAllocs([]Case{c}, Options{})
	kept := MergeAllocs(rep, rec)
	if len(kept.Rows[0].Rounds) != 0 || kept.Valid {
		t.Errorf("changed set kept its rounds: %+v", kept.Rows[0])
	}
	c.Refs = nil
	rec, _ = RecordAllocs([]Case{c}, Options{})
	if gone := MergeAllocs(rep, rec); gone.Schema != SchemaVersion {
		t.Errorf("schema = %d after dropping every reference", gone.Schema)
	}
	// Licence of a kept set follows the registry.
	c = multiCase(&log)
	c.Refs[1].License = "MIT"
	rec, _ = RecordAllocs([]Case{c}, Options{})
	if got := MergeAllocs(rep, rec); got.Rows[0].Refs[1].License != "MIT" || len(got.Rows[0].Rounds) != 4 {
		t.Errorf("licence not updated: %+v", got.Rows[0].Refs)
	}
}

func TestFirstDiff(t *testing.T) {
	nan := math.NaN()
	for _, c := range []struct {
		a, b []float64
		want int
	}{
		{[]float64{1, 2}, []float64{1, 2}, -1},
		{[]float64{1, 2}, []float64{1, 3}, 1},
		{[]float64{0}, []float64{math.Copysign(0, -1)}, 0},
		{[]float64{nan}, []float64{nan}, -1},
		{[]float64{1}, []float64{1, 2}, 1},
		{nil, nil, -1},
	} {
		if got := FirstDiff(c.a, c.b); got != c.want {
			t.Errorf("FirstDiff(%v, %v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCalibrateDiscardsTheFirstCall(t *testing.T) {
	// A JIT-like timer: its first call lasts 1 s, the others take 1 us per call.
	calls := 0
	tm := func(iters int) (time.Duration, error) {
		calls++
		if calls == 1 {
			return time.Second, nil
		}
		return time.Duration(iters) * time.Microsecond, nil
	}
	got, err := Calibrate(tm, 10*time.Millisecond)
	if err != nil || got < 10000 {
		t.Errorf("iters = %d, %v: the first call was used", got, err)
	}
	if _, err := Calibrate(func(int) (time.Duration, error) { return 0, errors.New("x") }, time.Millisecond); err == nil {
		t.Error("warm-up error lost")
	}
}

// spinFor returns a Go body that lasts ns nanoseconds per iteration, by busy waiting.
func spinFor(ns int64) func(int) {
	return func(iters int) {
		for end := time.Now().Add(time.Duration(ns * int64(iters))); time.Now().Before(end); {
		}
	}
}

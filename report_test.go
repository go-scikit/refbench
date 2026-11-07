package refbench

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleReport() *Report {
	r := &Report{
		Schema: SchemaVersion, Status: StatusMeasured, Date: "2026-11-07T12:00:00Z", Commit: "abc", Repo: "go-x/y",
		Go: "go1.27.3", Machine: "m", MaxLoad: 8, Valid: true,
		RefVersions: map[string]string{"numpy": "2.5.4", "blas": "accelerate"},
		Rows: []Row{
			{Name: "slow|row", Func: "F", Reference: "ref|x", Parity: ParityExact, Allocs: 2, GoIters: 10, RefIters: 10, Rounds: []Round{
				{LoadBefore: 1, LoadAfter: 2, GoFirst: true, GoNs: 100, RefNs: 10},
				{LoadBefore: 3, LoadAfter: 4, GoNs: 110, RefNs: 11},
			}},
			{Name: "fast", Func: "G", Reference: "ref", Parity: ParityTolerance, Rounds: []Round{
				{LoadBefore: 1, LoadAfter: 1, GoFirst: true, GoNs: 1, RefNs: 10},
			}},
		},
	}
	r.Finish()
	return r
}

func TestFinishAndWriteRoundTrip(t *testing.T) {
	r := sampleReport()
	if r.Rows[0].Verdict != Slower || r.Rows[1].Verdict != Faster || r.Rows[0].Ratio != 10 {
		t.Errorf("rows = %+v", r.Rows)
	}
	if r.Load.Min != 1 || r.Load.Max != 4 {
		t.Errorf("load = %+v", r.Load)
	}
	var b bytes.Buffer
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	js := b.String()
	back, err := ReadReport(&b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(Table(back), Table(r)) {
		t.Error("table differs after a JSON round trip")
	}
	// Derived fields are recomputed, never trusted.
	s := strings.Replace(js, `"verdict": "slower"`, `"verdict": "faster"`, 1)
	back, err = ReadReport(strings.NewReader(s))
	if err != nil || back.Rows[0].Verdict != Slower {
		t.Errorf("verdict trusted from the file: %v", err)
	}
	// An empty report finishes without NaN.
	e := &Report{}
	e.Finish()
	if e.Load != (LoadStats{}) {
		t.Errorf("load = %+v", e.Load)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriteErrors(t *testing.T) {
	if err := sampleReport().Write(failWriter{}); err == nil {
		t.Error("write error lost")
	}
	r := sampleReport()
	r.Rows[0].Allocs = math.NaN()
	if err := r.Write(&bytes.Buffer{}); err == nil {
		t.Error("NaN encoded")
	}
}

func TestReadReportRejects(t *testing.T) {
	var ok bytes.Buffer
	if err := sampleReport().Write(&ok); err != nil {
		t.Fatal(err)
	}
	good := ok.String()
	mut := func(old, new string) string { return strings.Replace(good, old, new, 1) }
	for name, in := range map[string]string{
		"empty":      ``,
		"unknown":    mut(`"schema": 1`, `"schema": 1, "extra": 1`),
		"trailing":   good + `{}`,
		"schema":     mut(`"schema": 1`, `"schema": 2`),
		"status":     mut(`"status": "measured"`, `"status": "wat"`),
		"maxload":    mut(`"max_load": 8`, `"max_load": -1`),
		"noname":     mut(`"name": "fast"`, `"name": ""`),
		"dup":        mut(`"name": "fast"`, `"name": "slow|row"`),
		"allocs":     mut(`"allocs": 2`, `"allocs": -2`),
		"load":       mut(`"load_before": 3`, `"load_before": -3`),
		"sample":     mut(`"go_ns": 110`, `"go_ns": -110`),
		"zeroref":    mut(`"ref_ns": 11`, `"ref_ns": 0`),
		"norounds":   `{"schema":1,"status":"measured","max_load":8,"rows":[{"name":"a","func":"F","parity":"none","allocs":0}]}`,
		"deferredR":  mut(`"status": "measured"`, `"status": "deferred"`),
		"loadafter":  mut(`"load_after": 4`, `"load_after": -4`),
		"loadbefore": mut(`"load_before": 1,`, `"load_before": -1,`),
	} {
		if _, err := ReadReport(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ReadReport(failReader{}); err == nil {
		t.Error("read error lost")
	}
	if _, err := ReadReport(&zeroReader{}); err == nil {
		t.Error("oversized input accepted")
	}
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("io") }

// zeroReader yields endless spaces.
type zeroReader struct{}

func (*zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

func FuzzReadReport(f *testing.F) {
	var b bytes.Buffer
	_ = sampleReport().Write(&b)
	f.Add(b.Bytes())
	f.Add([]byte(`{"schema":1,"status":"deferred","max_load":8,"rows":[{"name":"a","func":"F","parity":"none","allocs":1}]}`))
	f.Add([]byte(`{"schema":1,"status":"measured","max_load":8,"rows":[{"name":"a","func":"F","allocs":0,"rounds":[{"load_before":1,"load_after":1,"go_ns":1e308,"ref_ns":1e-308}]}]}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, in []byte) {
		r, err := ReadReport(bytes.NewReader(in))
		if err != nil {
			return
		}
		// Whatever is accepted renders and re-reads to the same table, never panics.
		var out bytes.Buffer
		if err := r.Write(&out); err != nil {
			return // Inf from a ratio of extreme samples is refused by encoding/json
		}
		back, err := ReadReport(&out)
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		if !bytes.Equal(Table(back), Table(r)) {
			t.Fatal("table differs after a round trip")
		}
	})
}

func TestTable(t *testing.T) {
	r := sampleReport()
	s := string(Table(r))
	for _, want := range []string{"# Performance table", "Do not edit", "- blas: accelerate\n- numpy: 2.5.4", "min 1.00, median 1.50, max 4.00; gate 8.00",
		"Run: valid", "slow\\|row", "ref\\|x", "| slower |", "| faster |", "10.00"} {
		if !strings.Contains(s, want) {
			t.Errorf("table lacks %q:\n%s", want, s)
		}
	}
	r.Valid = false
	s = string(Table(r))
	if !strings.Contains(s, "**invalid**") || !strings.Contains(s, "| invalid |") {
		t.Errorf("invalid run not marked:\n%s", s)
	}
	d := &Report{Status: StatusDeferred, Rows: []Row{{Name: "a", Func: "F", Parity: ParityNone, Allocs: 3}}}
	s = string(Table(d))
	if !strings.Contains(s, "**deferred**") || !strings.Contains(s, "| — | — | — | deferred |") {
		t.Errorf("deferred table:\n%s", s)
	}
}

func TestIssues(t *testing.T) {
	r := sampleReport()
	is, err := Issues(r)
	if err != nil || len(is) != 1 {
		t.Fatalf("issues = %v, %v", is, err)
	}
	if is[0].Title != "perf: slow|row is 10.00x slower than ref|x on one core" || is[0].Labels[0] != "perf" {
		t.Errorf("title = %q", is[0].Title)
	}
	for _, want := range []string{"1.00, 2.00, true", "2. 3.00, 4.00, false", "numpy: 2.5.4", "abc", "Gate"} {
		if !strings.Contains(is[0].Body, want) {
			t.Errorf("body lacks %q:\n%s", want, is[0].Body)
		}
	}
	r.Valid = false
	if _, err := Issues(r); !errors.Is(err, ErrInvalidRun) {
		t.Errorf("invalid run: %v", err)
	}
	d := &Report{Status: StatusDeferred}
	if _, err := Issues(d); !errors.Is(err, ErrInvalidRun) {
		t.Errorf("deferred run: %v", err)
	}
}

func TestParseExempt(t *testing.T) {
	in := "# Exempt\n\nprose\n- `Foo`: wrapper of Bar\n- Baz.Qux — no reference exists\n- `T.M` - cheap accessor\n- Plain: colon form\n"
	m, err := ParseExempt(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 4 || m["Foo"] != "wrapper of Bar" || m["Baz.Qux"] != "no reference exists" || m["T.M"] != "cheap accessor" || m["Plain"] != "colon form" {
		t.Errorf("m = %v", m)
	}
	for _, bad := range []string{"- `Foo`\n", "- `Foo: x\n", "- `` : x\n", "- Foo\n", "- `A`: x\n- `A`: y\n", "- : x\n"} {
		if _, err := ParseExempt(strings.NewReader(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	long := "- `A`: " + strings.Repeat("x", maxExemptLine+1)
	if _, err := ParseExempt(strings.NewReader(long)); err == nil {
		t.Error("overlong line accepted")
	}
}

func FuzzParseExempt(f *testing.F) {
	f.Add("- `Foo`: reason\n")
	f.Add("- Foo — reason\n- Bar.Baz: r\n")
	f.Add("- `\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		m, err := ParseExempt(strings.NewReader(s))
		if err != nil {
			return
		}
		for name, reason := range m {
			if name == "" || strings.TrimSpace(reason) == "" {
				t.Fatalf("accepted %q: %q", name, reason)
			}
		}
	})
}

const libSrc = `package lib

type T struct{}
type u struct{}
type G[X any] struct{}

func Exported() {}
func unexported() {}
func (T) Method() {}
func (*T) PtrMethod() {}
func (T) String() string { return "" }
func (T) MarshalJSON() ([]byte, error) { return nil, nil }
func (T) hidden() {}
func (u) Method() {}
func (G[X]) Gen() {}
func (*G[X]) GenPtr() {}
func (x (T)) Paren() {}
type P[A, B any] struct{}
func (P[A, B]) Pair() {}
`

func makeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "lib.go", libSrc)
	writeFile(t, root, "lib_test.go", "package lib\nfunc TestOnly() {}\n")
	writeFile(t, root, "sub/pkg/p.go", "package pkg\nfunc Deep() {}\n")
	writeFile(t, root, "cmd/tool/main.go", "package main\nfunc Main() {}\n")
	writeFile(t, root, "internal/x/x.go", "package x\nfunc Hidden() {}\n")
	writeFile(t, root, "bench/b.go", "package bench\nfunc B() {}\n")
	writeFile(t, root, ".hidden/h.go", "package h\nfunc H() {}\n")
	writeFile(t, root, "mainpkg/m.go", "package main\nfunc M() {}\n")
	return root
}

func TestPublicFuncs(t *testing.T) {
	got, err := PublicFuncs(makeRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	want := "Exported G.Gen G.GenPtr P.Pair T.Method T.Paren T.PtrMethod sub/pkg.Deep"
	if strings.Join(got, " ") != want {
		t.Errorf("got  %v\nwant %s", got, want)
	}
	bad := t.TempDir()
	writeFile(t, bad, "x.go", "package")
	if _, err := PublicFuncs(bad); err == nil {
		t.Error("syntax error accepted")
	}
	if _, err := PublicFuncs(filepath.Join(bad, "missing")); err == nil {
		t.Error("missing root accepted")
	}
}

func TestCheckCoverage(t *testing.T) {
	pub := []string{"A", "B", "C", "D"}
	cases := []Case{{Func: "A"}, {Func: "Typo"}, {Func: "D"}}
	cov := CheckCoverage(pub, cases, map[string]string{"B": "r", "Ghost": "r", "D": "r"})
	if cov.OK() || strings.Join(cov.Missing, ",") != "C" || strings.Join(cov.Unknown, ",") != "Ghost,Typo" || strings.Join(cov.Both, ",") != "D" {
		t.Errorf("cov = %+v", cov)
	}
	if !CheckCoverage([]string{"A"}, []Case{{Func: "A"}}, nil).OK() {
		t.Error("clean coverage not OK")
	}
}

func TestCheckAllocs(t *testing.T) {
	rep := &Report{Rows: []Row{{Name: "same", Allocs: 1}, {Name: "up", Allocs: 1}, {Name: "down", Allocs: 2}, {Name: "orphan"}}}
	var sink [][]byte
	mk := func(name string, n int) Case {
		c := goodCase(name)
		c.Go = func(int) {
			sink = sink[:0]
			for i := 0; i < n; i++ {
				sink = append(sink, make([]byte, 8))
			}
		}
		return c
	}
	_ = sink
	// append may allocate the slice header growth too: use fixed-size allocs via a local array.
	mk2 := func(name string, n int) Case {
		c := goodCase(name)
		c.Go = func(int) {
			for i := 0; i < n; i++ {
				sinkB = make([]byte, 8)
			}
		}
		return c
	}
	bad := CheckAllocs(rep, []Case{mk2("same", 1), mk2("up", 3), mk2("down", 1), mk2("new", 1)})
	joined := strings.Join(bad, "\n")
	for _, want := range []string{"up: 3 allocs per call, perf.json has 1 (regression)", "down: 1 allocs per call, perf.json has 2 (record the improvement)", "new: no row in perf.json", "orphan: a row in perf.json with no case"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
	if len(bad) != 4 {
		t.Errorf("bad = %v", bad)
	}
	_ = mk
}

var sinkB []byte

func ciRepo(t *testing.T) (string, []Case) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "lib.go", "package lib\nfunc A() {}\nfunc B() {}\n")
	c := goodCase("a")
	c.Func = "A"
	rep, err := RecordAllocs([]Case{c}, fixedOpts())
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := rep.Write(&b); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "docs/perf.json", b.String())
	writeFile(t, root, "docs/perf-table.md", string(Table(rep)))
	writeFile(t, root, "docs/perf-exempt.md", "- `B`: accessor\n")
	return root, []Case{c}
}

func TestRunCI(t *testing.T) {
	root, cases := ciRepo(t)
	if err := RunCI(CIConfig{Root: root, Cases: cases}); err != nil {
		t.Fatal(err)
	}
	// Hand-edited table.
	table := filepath.Join(root, "docs", "perf-table.md")
	orig, _ := os.ReadFile(table)
	writeFile(t, root, "docs/perf-table.md", string(orig)+"edit\n")
	if err := RunCI(CIConfig{Root: root, Cases: cases}); err == nil || !strings.Contains(err.Error(), "not what") {
		t.Errorf("hand edit: %v", err)
	}
	writeFile(t, root, "docs/perf-table.md", string(orig))
	// Missing exemption file: B becomes missing. Unknown and both are reported too.
	if err := os.Remove(filepath.Join(root, "docs", "perf-exempt.md")); err != nil {
		t.Fatal(err)
	}
	err := RunCI(CIConfig{Root: root, Cases: cases})
	if err == nil || !strings.Contains(err.Error(), "B has no case and no exemption") {
		t.Errorf("missing coverage: %v", err)
	}
	writeFile(t, root, "docs/perf-exempt.md", "- `B`: r\n- `A`: r\n- `Z`: r\n")
	err = RunCI(CIConfig{Root: root, Cases: cases})
	if err == nil || !strings.Contains(err.Error(), "Z is named but is not") || !strings.Contains(err.Error(), "A has a case and an exemption") {
		t.Errorf("unknown/both: %v", err)
	}
	// An allocation change.
	writeFile(t, root, "docs/perf-exempt.md", "- `B`: r\n")
	cases[0].Go = func(int) { sinkB = make([]byte, 8) }
	if err := RunCI(CIConfig{Root: root, Cases: cases}); err == nil || !strings.Contains(err.Error(), "regression") {
		t.Errorf("allocs: %v", err)
	}
}

func TestRunCIErrors(t *testing.T) {
	root, cases := ciRepo(t)
	if err := RunCI(CIConfig{Root: root, Cases: []Case{{}}}); err == nil {
		t.Error("invalid registry accepted")
	}
	if err := RunCI(CIConfig{Root: root, Cases: cases, JSON: filepath.Join(root, "nope.json")}); err == nil {
		t.Error("missing json accepted")
	}
	writeFile(t, root, "bad.json", "{")
	if err := RunCI(CIConfig{Root: root, Cases: cases, JSON: filepath.Join(root, "bad.json")}); err == nil {
		t.Error("bad json accepted")
	}
	if err := RunCI(CIConfig{Root: root, Cases: cases, Table: filepath.Join(root, "nope.md")}); err == nil {
		t.Error("missing table accepted")
	}
	writeFile(t, root, "ex.md", "- `B`\n")
	if err := RunCI(CIConfig{Root: root, Cases: cases, Exempt: filepath.Join(root, "ex.md")}); err == nil {
		t.Error("bad exemptions accepted")
	}
	// A directory where the exemption file should be: not "not exist".
	if err := RunCI(CIConfig{Root: root, Cases: cases, Exempt: filepath.Join(root, "docs")}); err == nil {
		t.Error("unreadable exemptions accepted")
	}
	if err := RunCI(CIConfig{Root: root, Cases: cases, Exempt: filepath.Join(root, "lib.go", "x")}); err == nil {
		t.Error("exemption path below a file accepted")
	}
	writeFile(t, root, "broken/b.go", "package")
	if err := RunCI(CIConfig{Root: root, Cases: cases}); err == nil {
		t.Error("unparsable source accepted")
	}
}

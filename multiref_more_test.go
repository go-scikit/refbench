package refbench

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestReadReportRejectsMutatedSchema2(t *testing.T) {
	for name, mut := range map[string]func(*Row){
		"go accuracy":      func(r *Row) { r.GoAccuracy.Coverage = 2 },
		"primary accuracy": func(r *Row) { r.PrimaryAccuracy.Points = -1 },
		"ref accuracy":     func(r *Row) { r.Refs[0].Accuracy.Coverage = -1 },
		"missing ns":       func(r *Row) { delete(r.Rounds[0].RefsNs, "boost:tgamma"); r.Rounds[0].RefsNs["x:y"] = 1 },
		"count":            func(r *Row) { delete(r.Rounds[0].RefsNs, "boost:tgamma") },
		"short order":      func(r *Row) { r.Rounds[0].Order = r.Rounds[0].Order[:2] },
		"duplicate order":  func(r *Row) { r.Rounds[0].Order = []string{"go", "go", "primary", "boost:tgamma"} },
		"unknown order":    func(r *Row) { r.Rounds[0].Order = []string{"go", "zz", "primary", "boost:tgamma"} },
	} {
		rep, _ := runMulti(t, 4)
		mut(&rep.Rows[0])
		var b bytes.Buffer
		if err := rep.Write(&b); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := ReadReport(&b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRefNameEqualToIDIsOmitted(t *testing.T) {
	c := Case{Name: "a", Func: "A", Go: func(int) {}, Parity: ParityNone, Refs: []Ref{{ID: "julia:x", Name: "julia:x", License: "MIT"}}}
	if rows := c.refRows(); rows[0].Name != "" || rows[0].label() != "julia:x" {
		t.Errorf("rows = %+v", rows)
	}
}

func TestSameRefIDs(t *testing.T) {
	a := []RefRow{{ID: "a:x"}, {ID: "b:y"}}
	if !sameRefIDs(a, a) || sameRefIDs(a, a[:1]) || sameRefIDs(a, []RefRow{{ID: "a:x"}, {ID: "b:z"}}) {
		t.Error("sameRefIDs")
	}
}

func TestTableRowsWithDifferentReferences(t *testing.T) {
	rep, _ := runMulti(t, 4)
	other := rep.Rows[0]
	other.Name = "other"
	other.Refs = append([]RefRow(nil), other.Refs...)
	other.Refs[0].ID = "fortran:amos"
	other.Rounds = nil
	rep.Rows = append(rep.Rows, other)
	tab := string(Table(rep))
	if !strings.Contains(tab, "fortran:amos") || !strings.Contains(tab, "| — |") {
		t.Errorf("table:\n%s", tab)
	}
	if sideName(Row{Reference: "scipy"}, SidePrimary) != "scipy" || sideName(Row{}, "zz:y") != "zz:y" {
		t.Error("sideName")
	}
}

func TestEvalerError(t *testing.T) {
	dir := t.TempDir()
	path, rows, sha, _ := WriteF64(dir, "x", []float64{1})
	w := fake(t, dir, []string{"f"}, func(line string) string {
		if strings.HasPrefix(line, "L") {
			return "ok " + sha
		}
		return "err domain"
	})
	if err := w.Load("x", path, 1, rows, sha); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Evaler("x", "f", dir+"/o", nil)(); err == nil {
		t.Error("worker error lost")
	}
}

func TestStartRBW1PipeErrors(t *testing.T) {
	old := execCommand
	defer func() { execCommand = old }()
	cfg := RBW1Config{Dir: t.TempDir(), Funcs: []string{"f"}}
	execCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, "x")
		c.Stdin = strings.NewReader("")
		return c
	}
	if _, err := StartRBW1(context.Background(), cfg, nil, "x"); err == nil {
		t.Error("stdin already set accepted")
	}
	execCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, "x")
		c.Stdout = io.Discard
		return c
	}
	if _, err := StartRBW1(context.Background(), cfg, nil, "x"); err == nil {
		t.Error("stdout already set accepted")
	}
}

func TestStartRBW1DefaultEnvIsSingleThread(t *testing.T) {
	helperRBW1(t, "ok")
	w, err := StartRBW1(context.Background(), RBW1Config{Dir: t.TempDir(), Funcs: []string{"f"}}, nil, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if !strings.Contains(strings.Join(w.cmd.Env, "\n"), "OMP_NUM_THREADS=1") {
		t.Error("the default environment is not SingleThreadEnv")
	}
}

func TestRunStopsOnATimerErrorInARound(t *testing.T) {
	var log []string
	c := multiCase(&log)
	calls := 0
	c.Refs[1].Timer = func(iters int) (time.Duration, error) {
		if calls++; calls > 3 { // calibration takes 3 calls (warm-up, 1, then the scaled count): the failure is in a timed round
			return 0, io.ErrUnexpectedEOF
		}
		return time.Duration(20000 * iters), nil
	}
	_, err := Run(context.Background(), []Case{c}, Options{Rounds: 4, MinTime: time.Millisecond, Load: func() (float64, error) { return 1, nil }})
	if err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("err = %v", err)
	}
}

func TestRunWithOnlyRetainsRefs(t *testing.T) {
	var log []string
	c := multiCase(&log)
	c2 := multiCase(&log)
	c2.Name = "other"
	rep, err := Run(context.Background(), []Case{c, c2}, Options{Rounds: 4, MinTime: time.Millisecond, Only: map[string]bool{"other": true}, Load: func() (float64, error) { return 1, nil }})
	if err != nil || len(rep.Rows) != 1 || rep.Rows[0].Name != "other" || len(rep.Rows[0].Refs) != 2 {
		t.Fatalf("%v %+v", err, rep)
	}
}

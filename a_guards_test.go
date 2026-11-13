package refbench

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file runs first (its name sorts before the others). Each test reaches a
// guard that protects a nil or an index, and asserts the REFUSAL. A mutant that
// removes the guard then fails an assertion here, instead of the suite going
// red only because the code after the missing guard crashes (a guard held by a
// crash is not held). Where the mutant necessarily crashes inside the call, the
// call is wrapped by mustNotPanic, which turns the crash into a failure that
// names the missing guard.

func mustNotPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s panicked: guard missing (%v)", what, r)
		}
	}()
	f()
}

func TestGuardReplyShapes(t *testing.T) {
	for _, in := range []string{"ns", "ns 1 2"} {
		mustNotPanic(t, "ParseRBW1Reply("+in+")", func() {
			if _, err := ParseRBW1Reply([]byte(in)); err == nil {
				t.Errorf("%q accepted", in)
			}
		})
	}
	dir := t.TempDir()
	path, rows, sha, err := WriteF64(dir, "x", []float64{1})
	if err != nil {
		t.Fatal(err)
	}
	reply := "ok"
	w := fake(t, dir, []string{"f"}, func(string) string { return reply })
	mustNotPanic(t, "Load with an 'ok' reply that has no digest", func() {
		if err := w.Load("x", path, 1, rows, sha); err == nil {
			t.Error("a load reply without a digest accepted")
		}
	})
	w.rows["x"] = 1
	mustNotPanic(t, "Eval with an 'ok' reply that has no counts", func() {
		if _, _, err := w.Eval("x", "f", filepath.Join(dir, "o")); err == nil {
			t.Error("an eval reply without counts accepted")
		}
	})
}

func TestGuardFiles(t *testing.T) {
	dir := t.TempDir()
	mustNotPanic(t, "WriteF64 with no column", func() {
		if _, _, _, err := WriteF64(dir, "a"); err == nil {
			t.Error("no column accepted")
		}
	})
	mustNotPanic(t, "WriteF64 with an empty column", func() {
		if _, _, _, err := WriteF64(dir, "a", []float64{}); err == nil {
			t.Error("an empty column accepted")
		}
	})
	mustNotPanic(t, "ReadF64 of a missing file", func() {
		if _, err := ReadF64(filepath.Join(dir, "missing.f64"), 1); err == nil {
			t.Error("a missing file read")
		}
	})
}

func TestGuardStarts(t *testing.T) {
	old := execCommand
	defer func() { execCommand = old }()
	execCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/nonexistent/refbench-guard")
	}
	mustNotPanic(t, "StartRBW1 with a bad run directory", func() {
		if _, err := StartRBW1(context.Background(), RBW1Config{Dir: "relative", Funcs: []string{"f"}}, nil, "x"); err == nil {
			t.Error("a relative run directory accepted")
		}
	})
	mustNotPanic(t, "StartJulia when the worker cannot start", func() {
		_, _, err := StartJulia(context.Background(), JuliaConfig{Pkgx: "pkgx", Version: "1.13.3", Project: "/p", Kernels: "/k.jl", Depot: "/d", Dir: t.TempDir(), Funcs: []string{"f"}})
		if err == nil {
			t.Error("a worker that cannot start accepted")
		}
	})
}

func TestGuardLabelsAndRefIDs(t *testing.T) {
	acc := &AccResult{MaxULPFin: 1, Coverage: 1}
	mustNotPanic(t, "Label without Go accuracy", func() {
		if got := Label(Faster, nil, acc, 0); got != "" {
			t.Errorf("label = %q", got)
		}
	})
	mustNotPanic(t, "Label without reference accuracy", func() {
		if got := Label(Faster, acc, nil, 0); got != "" {
			t.Errorf("label = %q", got)
		}
	})
	mustNotPanic(t, "Label without a verdict", func() {
		if got := Label("", acc, acc, 0); got != "" {
			t.Errorf("label = %q", got)
		}
	})
	a := []RefRow{{ID: "a:x"}, {ID: "b:y"}}
	mustNotPanic(t, "sameRefIDs of lists of different lengths", func() {
		if sameRefIDs(a, a[:1]) || sameRefIDs(a[:1], a) {
			t.Error("lists of different lengths are the same")
		}
	})
}

func TestGuardRunnerNils(t *testing.T) {
	var log []string
	c := multiCase(&log)
	c.Ref = nil
	mustNotPanic(t, "Run of a case without a primary timer", func() {
		if _, err := Run(context.Background(), []Case{c}, Options{}); err == nil || !strings.Contains(err.Error(), "no reference timer") {
			t.Errorf("err = %v", err)
		}
	})
	c = multiCase(&log)
	c.Accuracy, c.RefEval = nil, nil
	row := c.row(0)
	mustNotPanic(t, "scoreAccuracy without Accuracy", func() {
		if err := scoreAccuracy(c, &row); err != nil || row.GoAccuracy != nil {
			t.Errorf("%v %+v", err, row)
		}
	})
	c = multiCase(&log)
	c.RefEval, c.Refs[0].Eval = nil, nil
	row = c.row(0)
	mustNotPanic(t, "scoreAccuracy of sides without Eval", func() {
		if err := scoreAccuracy(c, &row); err != nil || row.PrimaryAccuracy != nil || row.Refs[0].Accuracy != nil || row.GoAccuracy == nil {
			t.Errorf("%v %+v", err, row)
		}
	})
}

func TestGuardNilCells(t *testing.T) {
	mustNotPanic(t, "ulpCell(nil)", func() {
		if ulpCell(nil) != "—" {
			t.Error("ulpCell(nil)")
		}
	})
	mustNotPanic(t, "covCell(nil)", func() {
		if covCell(nil) != "—" {
			t.Error("covCell(nil)")
		}
	})
	mustNotPanic(t, "accCell(nil)", func() {
		if accCell(nil) != "no accuracy" {
			t.Error("accCell(nil)")
		}
	})
}

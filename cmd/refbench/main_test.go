package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/go-scikit/refbench"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := RootCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func writeReport(t *testing.T, dir string, slower bool) string {
	t.Helper()
	g := 100.0
	if !slower {
		g = 1
	}
	rep := &refbench.Report{Schema: 1, Status: refbench.StatusMeasured, Date: "d", MaxLoad: 8, Valid: true, Rows: []refbench.Row{
		{Name: "Orient2D/random", Func: "Orient2D", Reference: "predicates.c", Parity: "exact", Rounds: []refbench.Round{{LoadBefore: 1, LoadAfter: 1, GoNs: g, RefNs: 10}}},
	}}
	p := filepath.Join(dir, "perf.json")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := rep.Write(f); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTableAndCheck(t *testing.T) {
	dir := t.TempDir()
	js := writeReport(t, dir, true)
	out, err := run(t, "table", js)
	if err != nil || !strings.Contains(out, "# Performance table") {
		t.Fatalf("%v %s", err, out)
	}
	tbl := filepath.Join(dir, "perf-table.md")
	if _, err := run(t, "table", "-o", tbl, js); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "check", js); err != nil || !strings.Contains(out, "matches") {
		t.Fatalf("%v %s", err, out)
	}
	if err := os.WriteFile(tbl, []byte("hand edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "check", js); err == nil || !strings.Contains(err.Error(), "not what") {
		t.Errorf("hand edit: %v", err)
	}
	if _, err := run(t, "check", "--table", filepath.Join(dir, "none.md"), js); err == nil {
		t.Error("missing table accepted")
	}
	for _, args := range [][]string{{"table", "missing.json"}, {"check", "missing.json"}, {"issues", "missing.json"}, {"table"}, {"table", "-o", filepath.Join(dir, "no", "such", "x.md"), js}} {
		if _, err := run(t, args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestIssues(t *testing.T) {
	dir := t.TempDir()
	js := writeReport(t, dir, true)
	out, err := run(t, "issues", js)
	if err != nil || !strings.Contains(out, "title: perf: Orient2D/random is 10.00x slower than predicates.c on one core") || !strings.Contains(out, "1 issue draft(s)") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := run(t, "issues", "--out", dir, js); err != nil {
		t.Fatal(err)
	}
	title, err := os.ReadFile(filepath.Join(dir, "orient2d-random.title.txt"))
	if err != nil || !strings.HasPrefix(string(title), "perf: Orient2D") {
		t.Errorf("%v %q", err, title)
	}
	if _, err := os.Stat(filepath.Join(dir, "orient2d-random.md")); err != nil {
		t.Error(err)
	}
	if _, err := run(t, "issues", "--out", filepath.Join(dir, "missing"), js); err == nil {
		t.Error("missing directory accepted")
	}
	// Body write fails when a directory has the body's name.
	if err := os.Mkdir(filepath.Join(dir, "orient2d-random.md.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	blocked := t.TempDir()
	if err := os.Mkdir(filepath.Join(blocked, "orient2d-random.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "issues", "--out", blocked, js); err == nil {
		t.Error("blocked body accepted")
	}
	// A run with nothing slower drafts nothing; a deferred run refuses.
	dir2 := t.TempDir()
	js2 := writeReport(t, dir2, false)
	if out, err := run(t, "issues", js2); err != nil || !strings.Contains(out, "0 issue draft(s)") {
		t.Errorf("%v %s", err, out)
	}
	if err := os.WriteFile(js2, []byte(`{"schema":1,"status":"deferred","max_load":8,"rows":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "issues", js2); err == nil {
		t.Error("deferred run accepted")
	}
}

func TestLoad(t *testing.T) {
	old := readLoad
	defer func() { readLoad = old }()
	readLoad = func() (float64, error) { return 7.99, nil }
	if out, err := run(t, "load"); err != nil || out != "7.99\n" {
		t.Errorf("%v %q", err, out)
	}
	readLoad = func() (float64, error) { return 8, nil }
	if _, err := run(t, "load"); err == nil || !strings.Contains(err.Error(), "deferred") {
		t.Errorf("gate: %v", err)
	}
	if _, err := run(t, "load", "--max", "9"); err != nil {
		t.Error(err)
	}
	readLoad = func() (float64, error) { return 0, errors.New("no load") }
	if _, err := run(t, "load"); err == nil {
		t.Error("read error lost")
	}
}

func TestMainAndVersion(t *testing.T) {
	oldExit, oldArgs := osExit, os.Args
	defer func() { osExit, os.Args = oldExit, oldArgs }()
	code := -1
	osExit = func(c int) { code = c }
	os.Args = []string{"refbench", "table", "missing.json"}
	main()
	if code != 1 {
		t.Errorf("exit code = %d", code)
	}
	code = -1
	os.Args = []string{"refbench", "--version"}
	main()
	if code != -1 {
		t.Errorf("--version exited with %d", code)
	}
	oldBI := buildInfo
	defer func() { buildInfo = oldBI }()
	buildInfo = func() (*debug.BuildInfo, bool) { return &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, true }
	if version() != "v1.2.3" {
		t.Errorf("version = %q", version())
	}
	buildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
	if version() != "(devel)" {
		t.Errorf("version = %q", version())
	}
}

package refbench

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestJuliaWorkerReal runs workers/julia/worker.jl in the pinned Julia, through
// pkgx, with kernels that need no package. It never runs in CI (no Julia
// there): set REFBENCH_JULIA=1 on a host that has pkgx and network access to
// fetch julialang.org once.
func TestJuliaWorkerReal(t *testing.T) {
	if os.Getenv("REFBENCH_JULIA") == "" {
		t.Skip("set REFBENCH_JULIA=1 (needs pkgx; runs the real Julia worker)")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	kernels := filepath.Join(dir, "kernels.jl")
	src := `const KERNELS = Dict{String,Function}(
    "sqrt" => x -> sqrt(x),
    "add" => (a, b) -> a + b,
    "dom" => x -> x < 0 ? throw(DomainError(x)) : x,
)
`
	if err := os.WriteFile(kernels, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	project, depot := filepath.Join(dir, "project"), filepath.Join(dir, "depot")
	for _, d := range []string{project, depot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := filepath.Join(dir, "run")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	w, v, err := StartJulia(context.Background(), JuliaConfig{Pkgx: "pkgx", Version: "1.13.3", Project: project, Kernels: kernels,
		Depot: depot, Dir: run, Funcs: []string{"sqrt", "add", "dom"}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if v["julia/julia"] != "1.13.3" || v["julia/threads"] != "1" {
		t.Errorf("versions = %v", v)
	}
	x := []float64{1, 4, 9, -1}
	path, rows, sha, err := WriteF64(run, "x", x)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Load("x", path, 1, rows, sha); err != nil {
		t.Fatal(err)
	}
	path2, _, sha2, err := WriteF64(run, "xy", x, x)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Load("xy", path2, 2, rows, sha2); err != nil {
		t.Fatal(err)
	}
	var nf, raised int
	got, err := w.Evaler("x", "dom", filepath.Join(run, "o.f64"), func(a, b int) { nf, raised = a, b })()
	if err != nil || nf != 1 || raised != 1 || !math.IsNaN(got[3]) || got[2] != 9 {
		t.Errorf("dom: %v %d %d %v", got, nf, raised, err)
	}
	if got, err = w.Evaler("xy", "add", filepath.Join(run, "a.f64"), nil)(); err != nil || got[1] != 8 {
		t.Errorf("add: %v %v", got, err)
	}
	if _, err := Calibrate(w.Timer("xy", "add"), 10*time.Millisecond); err != nil {
		t.Error(err)
	}
	// A kernel of the wrong arity raises on every point: refused, not an all-NaN column.
	if _, err := w.Evaler("xy", "sqrt", filepath.Join(run, "b.f64"), nil)(); err == nil || !strings.Contains(err.Error(), "every point") {
		t.Errorf("wrong arity: %v", err)
	}
	// The worker's own refusals, bypassing the client's: a product that wraps
	// in Int64, a path outside the run directory, a function outside its table.
	for _, line := range []string{"L y " + path + " 3037000500 3037000500", "L y " + path + " 1 536870913", "L y /etc/passwd 1 1", "T x nofn 1", "W nokey sqrt", "E x sqrt /tmp/o", "zz"} {
		if _, err := w.ask(strings.Split(line, " ")...); err == nil {
			t.Errorf("the Julia worker accepted %q", line)
		}
	}
}

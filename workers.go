package refbench

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// JuliaWorkerSource is the Julia program of the rbw1 worker. It reads a kernels
// file (a Dict of scalar functions) named by its first argument.
//
//go:embed workers/julia/worker.jl
var JuliaWorkerSource string

// CompiledHarnessSource is the C++ harness of the compiled rbw1 workers (C,
// C++, Fortran, Boost.Math): link it with a file defining rbw1_kernels.
//
//go:embed workers/compiled/rbw1.cpp
var CompiledHarnessSource string

// JuliaConfig describes a Julia reference worker.
type JuliaConfig struct {
	// Pkgx is the pkgx executable; Version the pinned Julia, "1.13.3": the
	// julialang.org binary through pkgx, never brew.
	Pkgx, Version string
	// Project is the directory with the pinned Project.toml and Manifest.toml
	// (exact versions and git-tree-sha1 content hashes).
	Project string
	// Kernels is the kernels file (a Dict KERNELS of scalar functions).
	Kernels string
	// Depot is JULIA_DEPOT_PATH: a directory of the tool (a user cache), where
	// `refbench prepare` instantiated and precompiled the project. The worker
	// runs with JULIA_PKG_OFFLINE=true: nothing is downloaded during a run.
	Depot string
	// Dir is the run directory, Funcs the kernels the cases may ask for.
	Dir   string
	Funcs []string
}

// JuliaArgs returns the arguments of the pkgx command that runs the worker:
//
// (pkgx 2.11 rejects the "@=" exact form of the design with "invalid semver";
// the version is pinned by what the worker reports, which StartJulia checks.)
//
//	pkgx +julialang.org@<version> -- julia --startup-file=no --history-file=no -t 1 --project=<dir> -e <worker> <kernels>
func JuliaArgs(c JuliaConfig) []string {
	return []string{"+julialang.org@" + c.Version, "--", "julia",
		"--startup-file=no", "--history-file=no", "-t", "1", "--project=" + c.Project,
		"-e", JuliaWorkerSource, c.Kernels}
}

// JuliaEnv returns the environment of a Julia worker: SingleThreadEnv plus the
// Julia thread counts, the depot and the offline package mode. What the worker
// observed is checked after the start; these variables are not trusted.
func JuliaEnv(c JuliaConfig) []string {
	return append(SingleThreadEnv(),
		"JULIA_NUM_THREADS=1", "JULIA_NUM_GC_THREADS=1", "JULIA_DEPOT_PATH="+c.Depot, "JULIA_PKG_OFFLINE=true")
}

// StartJulia starts the Julia worker, asks for its versions and refuses it
// unless it runs the pinned Julia on one thread. The versions (Julia, LLVM,
// every package of the Manifest with its git-tree-sha1) are returned under the
// namespace "julia/" for Options.RefVersions.
func StartJulia(ctx context.Context, c JuliaConfig) (*RBW1, map[string]string, error) {
	w, err := StartRBW1(ctx, RBW1Config{Dir: c.Dir, Funcs: c.Funcs}, JuliaEnv(c), c.Pkgx, JuliaArgs(c)...)
	if err != nil {
		return nil, nil, err
	}
	v, err := w.Versions()
	if err == nil {
		err = CheckSingleThread(v)
	}
	if err == nil && v["julia"] != c.Version {
		err = fmt.Errorf("rbw1: the worker runs Julia %q, the pin is %q", v["julia"], c.Version)
	}
	if err != nil {
		_ = w.Close()
		return nil, nil, err
	}
	return w, NamespaceVersions("julia", v), nil
}

// ReleaseFlags are the default flags of a compiled reference: the library's
// normal release build (-O2, the compiler's own FP contraction, no
// -march=native), not the parity build of NoFMAFlags. The flags are recorded
// and a different flag set is a different Ref.ID.
var ReleaseFlags = []string{"-O2"}

// BuildWorker compiles a compiled rbw1 worker: cc flags... -o out sources...
// (the harness, a kernel table and the kernels, as sources or objects; a
// source that starts with "-", such as -lgfortran, is passed through after the
// others). It returns what the record needs: the SHA-256 of every source under
// "src/<base name>" and the flags under "flags".
func BuildWorker(ctx context.Context, cc, out string, flags, sources []string) (map[string]string, error) {
	if len(sources) == 0 {
		return nil, errors.New("build worker: no sources")
	}
	rec := map[string]string{"flags": strings.Join(flags, " ")}
	for _, s := range sources {
		if strings.HasPrefix(s, "-") { // a library (-lgfortran): after the objects, so that it links
			rec["flags"] += " " + s
			continue
		}
		b, err := os.ReadFile(s)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		rec["src/"+filepath.Base(s)] = "sha256:" + hex.EncodeToString(sum[:])
	}
	if err := BuildC(ctx, cc, out, flags, sources); err != nil {
		return nil, err
	}
	return rec, nil
}

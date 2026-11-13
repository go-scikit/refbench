# refbench

The shared reference-benchmark tool of the go-scikit stack: it measures a Go
function against its reference (NumPy, SciPy, a C library) **on one core**, in
**interleaved rounds**, **gated on the host's load**, and writes
`docs/perf-table.md` and `docs/perf.json`. It is wave W0 of the
[performance campaign](https://github.com/go-scikit/roadmap/blob/main/docs/performance-campaign.md)
(go-scikit/roadmap#27); the rules quoted below are that page's section 1.

It is a library first (cgo-free, importable from a `perf_test.go`), with a small
cobra command for the files it writes.

```go
import "github.com/go-scikit/refbench"
```

## Install

```sh
go get github.com/go-scikit/refbench@v0.1.0                       # the library, and rbtest
GOBIN=~/.local/bin go install github.com/go-scikit/refbench/cmd/refbench@v0.1.0
```

## What a run does

- **One core on both sides.** `Run` sets `GOMAXPROCS=1` around the whole run
  (restored after); a reference worker starts under `SingleThreadEnv()`:
  `OMP_NUM_THREADS`, `OPENBLAS_NUM_THREADS`, `MKL_NUM_THREADS`,
  `VECLIB_MAXIMUM_THREADS` at 1.
- **One statistic.** Each side is a loop calibrated to the same minimum
  duration (`Options.MinTime`), timed inside its own process (the pipe is not in
  the interval). A round is one sample per side; the verdict is on the median
  over rounds, with the quartiles kept.
- **Interleaved, alternating.** For every case, round *r* runs Go then reference
  when *r* is even, reference then Go when it is odd. Never all of one side
  first. A reference error stops the run: a series with a hole is biased, not
  smaller.
- **Load gate.** The 1-minute load (`go-fleettools/fleettools/loadavg`) is read
  before every round, stored with the round, and read again after it. At or
  above `MaxLoad` (default 8) `Run` waits up to `LoadTimeout`
  (`OnLoad: refuse` checks once and refuses); a gate that does not open returns
  `ErrOnLoad` when no row could be measured at all. A round that ends above the
  gate is discarded and **re-run** (the next gate check waits until the load is
  back under the gate; the round keeps its place in the Go/reference
  alternation), at most `MaxRetries` times per row (default 10); past the cap that
  row alone is deferred. Retries are counted per row in `perf.json`.
- **Partial runs.** A row is *valid* when all its kept rounds started and ended
  under the gate; valid rows are written on their own, `perf.json` lists the
  deferred ones (`deferred`), and the table says `partial: N of M rows valid`.
  A gate that stays closed past `LoadTimeout` stops the run: the rows measured so
  far are kept, the rest are deferred. `Merge(prev, next)` combines a resumed run
  (`Options.Only` restricts it to `Pending(prev, cases)`; `CheckResume` refuses
  another commit, Go version, reference versions or gate) with the previous
  report; a run is complete when no row is deferred. `refbench issues` drafts
  from the valid rows only.
- **Verdict.** `ratio = Go / reference` (median); *slower* when the Go first
  quartile is above the reference third quartile, *faster* when the Go third
  quartile is below the reference first quartile, *tie* otherwise.
- **Deferred.** `RecordAllocs` writes a report with allocation counts and no
  timing (`status: deferred`), for a repository whose host cannot give a quiet
  window yet. Allocation counts are exact under any load; times are not.

## Registering cases

```go
var cases = []refbench.Case{{
	Name:      "Orient2D/random",
	Func:      "Orient2D",             // the public function measured
	Reference: "predicates.c",
	Parity:    refbench.ParityExact,
	Go:        func(iters int) { /* call it iters times */ },
	Ref:       cRef,                   // a refbench.Timer: a cgo loop, or worker.Timer(...)
}}

rep, err := refbench.Run(ctx, cases, refbench.Options{Commit: sha, Repo: "go-geometry/predicates"})
```

A `Timer` runs the reference `iters` times and returns the elapsed time measured
in the reference's own process.

### Python references

`refbench.StartPython(ctx, uv, "refs.txt")` starts one long-lived worker,

```
uv run --no-project --python 3.12.12 --with-requirements refs.txt python -I -c <worker>
```

speaking JSON lines (`pyworker.py` is embedded as `PyWorkerSource`).
`worker.Timer(setup, call, n)` times `iters` calls of the Python expression
`call` on the arguments `setup` builds for size `n`; `worker.DataHash` returns
the SHA-256 of those arguments, to compare with the Go side's data;
`worker.RefVersions` reports numpy, scipy, statsmodels, mpmath and the BLAS for
the table header. `refs.txt` is committed with exact versions and hashes
(`uv pip compile refs.in --generate-hashes`): numpy 2.5.4, scipy 1.18.1,
statsmodels 0.15.0, mpmath 1.4.1.

### C references

`BuildC(ctx, cc, out, refbench.NoFMAFlags, sources)` compiles a worker from
committed sources (`-O2 -ffp-contract=off`), so the library never needs cgo. A
repository that already has a cgo module may keep it and time it through the
same runner (go-geometry/predicates `bench/` does).

## Several references per case (schema 2)

SciPy is not the only bar: Julia, Boost.Math and the Fortran libraries are
references too. `Case.Ref` stays the primary reference (existing tables and
sentences stay true); `Case.Refs` adds more, timed in the same rounds
([design](docs/design/multiref.md); the maintainer decisions of
[#15](https://github.com/go-scikit/refbench/issues/15) override it):

```go
c.Refs = []refbench.Ref{
	{ID: "julia:Bessels.besselk", License: "MIT", Timer: julia.Timer("kv", "bessels_kv"), Eval: julia.Evaler("kv", "bessels_kv", out, nil)},
	{ID: "boost:cyl_bessel_k", License: "BSL-1.0", Timer: boost.Timer("kv", "kv")},
}
c.RefEval = scipyEval // the primary reference's values on the accuracy points
c.Accuracy = &refbench.Accuracy{Go: goEval, Score: ulpAgainstMpmath, Budget: 4}
```

- **Rounds rotate.** With *k* sides (Go, the primary, the extra ones) round *r*
  runs them rotated by *r* mod *k*, so every side takes every position once per
  *k* rounds, and `Rounds` is rounded up to a multiple of *k*. With two sides it
  is exactly the alternation above. A round above the gate is discarded for all
  sides.
- **One verdict per reference**, the rule above, plus a headline against the
  **fastest side** (`best`, the primary included). **A gap against any faster
  reference is a defect, even when that reference is less accurate**: `refbench
  issues` drafts one issue per case when Go is slower than any side, titled after
  the fastest of them, with every reference, its license, version, ratio and max
  ulp in the body.
- **Accuracy in the same row.** `Accuracy.Score` is the repository's own scorer
  (special: ulp against the mpmath `hi+lo` fixtures); Go, the primary
  (`Case.RefEval`) and every `Ref.Eval` are scored on the same points, once,
  ungated, before the rounds. `AccResult` carries `MaxULP` (+Inf, written
  `"inf"`, when a finite value was expected and NaN came back), `MaxULPFin` and
  `Coverage`, so a reference that refuses half the domain does not look
  accurate. Labels: `ahead`, `faster-less-accurate` (an accuracy defect),
  `behind`, `tradeoff` (slower than a reference that is less accurate than Go's
  stated `Budget`: still a perf defect). The first multi-reference run must
  reproduce the fixtures' SciPy column bit for bit (`FirstDiff`), which proves the
  inputs reach the workers unaltered.
- **Schema.** A repository with no `Refs` keeps writing schema 1, byte for byte
  (a golden test pins it). One with `Refs` writes **schema 2**, which an older
  refbench refuses (`perf.json: schema 2, this build reads 1`): bump refbench in
  the same PR. `Round` carries a map and a slice, so it is no longer comparable
  with `==` (use `reflect.DeepEqual`). The table gains a second section,
  "Against other references".
- **CI stays cheap.** `rbtest` starts no runtime: besides allocations, table and
  coverage it checks that every `Ref.ID` (`<kind>:<name>`, unique) has an entry in
  its row, that every reference has a license, that none is GPL unless its kind
  is a black-box kind (`r`, `gsl`), and that the table regenerates from the JSON.
- **Calibrate** discards one untimed first call: a reference that compiles on its
  first call (Julia) would otherwise last longer than `MinTime` at `iters = 1`
  and settle on single-call samples.

### The rbw1 worker protocol

One request line, one reply line, plain ASCII, at most 4096 bytes (a longer line
is refused, never buffered), for Julia, R, C++ and Fortran (Python keeps JSON):

```
L <key> <path> <cols> <rows>   load float64 little-endian column-major  -> ok <sha256 of the bytes read>
W <key> <fn>                   one untimed call (JIT warm-up)           -> ok
T <key> <fn> <iters>           time iters calls inside the worker       -> ns <float>
E <key> <fn> <outpath>         evaluate once, write rows float64        -> ok <n_nonfinite> <n_raised>
V                              versions and observed threading          -> v k=v k=v ...
```

`<fn>` is a name in a fixed table in the worker (`KERNELS` in Julia,
`rbw1_kernels` in the harness): nothing read from the pipe is evaluated. Paths
must lie in the run directory (`DataDir`: `os.UserCacheDir()/refbench/<repo>/<commit>`,
never `/tmp`), sizes are bounded (256 MiB). `WriteF64` writes an argument file and
returns its SHA-256, `RBW1.Load` compares it with the worker's and refuses a
mismatch naming both digests before anything is timed. `RBW1.Timer` runs one
untimed `W` before the first timed call. `CheckSingleThread` refuses a worker
whose `V` reply does not show `threads=1` (and every `*_threads` at 1): the
environment variables are asked for, what the worker observed is checked.
`ParseRBW1Reply` has a fuzz target and a committed corpus.

### Julia references

`refbench.StartJulia(ctx, refbench.JuliaConfig{...})` runs the julialang.org
binary through **pkgx** (never brew), pinned (`JuliaConfig.Version`, refused
unless the worker reports exactly it):

```
pkgx +julialang.org@1.13.3 -- julia --startup-file=no --history-file=no -t 1 --project=<dir> -e <worker> <kernels.jl>
```

with `JULIA_NUM_THREADS=1`, `JULIA_NUM_GC_THREADS=1`, `JULIA_PKG_OFFLINE=true` and
`JULIA_DEPOT_PATH` set. The project (`Project.toml`, `Manifest.toml` with exact
versions and git-tree-sha1 content hashes) is committed in the consumer
repository; the depot is instantiated and precompiled once, outside any gated
run. `kernels.jl` defines `KERNELS = Dict("name" => scalar_function, ...)`; the
timed body is `out .= f.(args...)` in a function barrier on typed
`Vector{Float64}`s, with no `@fastmath`. The versions (Julia, LLVM, every
package of the Manifest with its tree hash) come back under `julia/...` keys for
`Options.RefVersions`, so `-perf.resume` refuses a different set.

### Compiled references (C, C++, Fortran, Boost.Math)

`workers/compiled/rbw1.cpp` (embedded as `CompiledHarnessSource`) is the harness
of a small stdin/stdout worker; a program adds a kernel table
(`rbw1_kernels`) and the kernels, in C++ or in Fortran (`bind(C)`); see
`workers/compiled/examples`. `BuildWorker(ctx, cc, out, refbench.ReleaseFlags,
sources)` compiles it (`-O2`, the compiler's own FP contraction, no
`-march=native`: the reference's normal release build; a different flag set is a
different `Ref.ID`) and returns the SHA-256 of every source and the flags for
`ref_versions`. R is not wired yet; when it comes it is `Rscript` only, GPL code is
never linked or read.

## CI mode: allocations, table, denominator

No Python, no timing, on every pull request. `rbtest` is the `go test` glue,
written once:

```go
func TestPerf(t *testing.T) {
	rbtest.Perf(t, refbench.CIConfig{Root: "..", Cases: cases}, refbench.Options{Repo: "go-x/y"})
}
```

```text
go test -run TestPerf .                       CI mode (below)
go test -run TestPerf -cpu 1 -perf.run .      measure; write docs/perf.json and docs/perf-table.md
go test -run TestPerf -perf.record .          allocation counts only; recorded timings are kept
go test -c -o perf.test . && ./perf.test -test.cpu 1 -test.run 'TestPerf$' -perf.run -perf.onload=wait -perf.wait=2h
./perf.test -test.cpu 1 -test.run 'TestPerf$' -perf.resume -perf.onload=wait -perf.wait=2h
```

`-perf.run` refuses at a load at or above the gate (`-perf.onload=wait
-perf.wait=1h` waits instead; `-perf.wait` without `-perf.onload=wait` fails
with a message naming the missing flag). A round that ends above the gate is
re-run (`-perf.retries`, default 10 per row, then the row is deferred). When
every row is valid the test passes; when some rows are deferred the files are
still written (valid rows, the `deferred` list, the table's `partial: N of M
rows valid`) and the test **fails** with `partial`, so an incomplete run never
reads as `ok`. `-perf.resume` re-measures only the deferred rows of the existing
`docs/perf.json` and merges them, refusing (with the field and both values) when
the commit (`-perf.commit`), the Go version, the reference versions or the gate
differ; run it until no row is deferred. A run that measured nothing at all
fails and writes nothing. `-perf.record` updates only the allocation counts
(timings, date and commit of the measurement are kept; a new case becomes a
deferred row). Without `-perf.run` nothing is timed and the test passes
quietly. Other flags: `-perf.rounds`, `-perf.mintime`, `-perf.maxload`,
`-perf.commit`, `-perf.machine`.

| flag | meaning |
|---|---|
| `-perf.commit <sha>` | optional; must equal the full 40-character `git rev-parse HEAD` of `<root>`, and the tree must be clean (`bench/go.mod` may differ only in `require` lines of other modules, with the library's `replace` unchanged; `bench/go.sum` only alongside it). Anything else refuses, naming both values or the dirty paths. Without git the flag refuses. |
| (flag omitted) | the commit is filled in from HEAD, suffixed `-dirty-<8 hex digest of the changes>` when the tree is, so `-perf.resume` and `-perf.record` refuse another tree; with no git it stays empty |

Nothing is written unless `<root>/go.mod` (the repository's `..` from a `bench/`
module) declares the parent of the running binary's module path, or the module
itself: a binary of one repository run from another's `bench/` directory refuses,
naming both module paths. `refbench.RunCI` is what the default mode calls:

0. with `Case.Refs`: every reference has an entry in its row, a license, and no
   GPL outside a black-box kind (see above);
1. every case's allocations per call must equal `docs/perf.json`'s (an increase
   is a regression; a decrease must be recorded);
2. `docs/perf-table.md` must be what `docs/perf.json` generates (no hand edits);
3. every public function (exported functions and methods of exported types, no
   `internal`, `cmd`, `tools`, `bench`, `benchmarks`, `examples`, `testdata`,
   no `main`, no `Error`/`String`/`Format`/`Marshal*`/`Unmarshal*`/`Unwrap`/`Is`)
   has a case or is named, with a reason, in `docs/perf-exempt.md`
   (``- `Func`: reason``); names that are not public functions are refused too.

## The command

```
refbench table <perf.json> [-o docs/perf-table.md]   render the table
refbench check <perf.json> [--table docs/perf-table.md]   fail on a hand-edited table
refbench issues <perf.json> [--out dir]              draft one issue per slower row
refbench load [--max 8]                              print the load; fail at or above the gate
```

`issues` drafts, it does not file: each slower row of a valid, measured run gives
`perf: <case> is <ratio>x slower than <reference> on one core`, label `perf`, a
body with the row, the machine, the load of every round, the reference versions
and the commit (`<case>.title.txt` and `<case>.md` with `--out`, for
`gh issue create --title ... --body-file`). A deferred report, or one with no
valid row, drafts nothing; a partial one drafts from its valid rows.

## Not yet (multi-reference, step 1)

- Restricting a run to some kinds (`-perf.refs`) and `case@refID` in the
  deferred list; the Python `L` request (the NumPy `setup` stays).
- The R worker; a shared home for the per-library kernels (Boost, AMOS,
  QUADPACK, MINPACK); `refbench prepare`.

## Not in v0.1.0

- Closing or commenting on an issue whose row is no longer slower.
- Expected allocation counts in the registry (perf.json is the record).
- The all-cores column.
- `loadavg` comes from go-fleettools/fleettools, a module that carries the whole
  operations toolbox; it belongs in a module of its own, and refbench imports it
  until it moves.

## Licence

BSD 3-Clause.

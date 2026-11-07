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
  `ErrOnLoad` and no report. A round that ends above the gate marks the report
  `valid: false`: no verdict, and `refbench issues` files nothing.
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
go test -run TestPerf -perf.record .          allocation counts only (status: deferred)
```

`-perf.run` refuses at a load at or above the gate (`-perf.onload=wait
-perf.wait=1h` waits instead) and then skips the test with the reason and
writes nothing. Other flags: `-perf.rounds`, `-perf.mintime`, `-perf.maxload`,
`-perf.commit`, `-perf.machine`. `refbench.RunCI` is what the default mode calls:

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
`gh issue create --title ... --body-file`). A deferred or invalid run drafts
nothing.

## Not in v0.1.0

- Closing or commenting on an issue whose row is no longer slower.
- Expected allocation counts in the registry (perf.json is the record).
- The all-cores column.
- `loadavg` comes from go-fleettools/fleettools, a module that carries the whole
  operations toolbox; it belongs in a module of its own, and refbench imports it
  until it moves.

## Licence

BSD 3-Clause.

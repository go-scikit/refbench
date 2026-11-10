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

Nothing is written unless `<root>/go.mod` (the repository's `..` from a `bench/`
module) declares the parent of the running binary's module path, or the module
itself: a binary of one repository run from another's `bench/` directory refuses,
naming both module paths. `refbench.RunCI` is what the default mode calls:

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

## Not in v0.1.0

- Closing or commenting on an issue whose row is no longer slower.
- Expected allocation counts in the registry (perf.json is the record).
- The all-cores column.
- `loadavg` comes from go-fleettools/fleettools, a module that carries the whole
  operations toolbox; it belongs in a module of its own, and refbench imports it
  until it moves.

## Licence

BSD 3-Clause.

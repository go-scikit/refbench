# refbench: several references per case (Julia, R, Fortran, Boost.Math)

Status: design, 2026-11-13. Step 1 (core, rbw1, Julia worker, compiled-worker
skeleton) is implemented in the PR that adds this file; later steps are not.

> **Binding override.** The "Decisions" section of
> [go-scikit/refbench#15](https://github.com/go-scikit/refbench/issues/15)
> (maintainer, 2026-11-13) overrides this document wherever they differ. The
> differences known at step 1:
>
> - **A gap against ANY faster reference is a defect**, even when that
>   reference is less accurate. Section 3.2 says the headline is the best among
>   the references whose accuracy was scored; the decision says the headline is
>   against the fastest reference, and a perf issue is drafted whenever one is
>   faster (the `tradeoff` label only describes it). Implemented: `Row.Best` is
>   the fastest side by median, the primary included; `refbench issues` drafts
>   one issue per case when Go is slower than any side, titled after the
>   fastest side Go is slower than.
> - **Q1-Q8 are answered by the decisions**: Julia from the pinned pkgx
>   binary (Q2), R through `Rscript` only (Q3), `tradeoff` is a perf defect
>   (Q4), SciPy stays primary (Q8).
> - **Step 1 leaves out** the per-kind restriction (`-perf.refs`, `case@refID`
>   in the deferred list, section 3.1) and the Python `L` request (section 3.4):
>   both need a consumer to design against. The decision that the first
>   multi-reference run must reproduce the fixtures' SciPy column bit for bit is
>   **not enforced in step 1** (`FirstDiff` exists, no gate calls it: the Python
>   worker has no `L` yet); it is deferred to step 2 with a planted negative
>   control.
> - **Round order.** Section 3.1's plain rotation keeps first-order carryover
>   (Go always follows the same side); the rounds follow a Williams design
>   instead (`2k` orders for odd `k`). Labels are judged on `MaxULPFin`, not on
>   `MaxULP`.
> - **pkgx syntax.** pkgx 2.11 rejects `+julialang.org@=1.13.3` ("invalid
>   semver"); the worker is started with `+julialang.org@1.13.3` and
>   `StartJulia` refuses it unless it reports exactly the pinned version.
> - `Round` is no longer a comparable struct (it carries a map and a slice):
>   compare rounds with `reflect.DeepEqual`.

Subject: the user asked to go past SciPy: "on peut ne pas se limiter à dépasser
scipy; si d'autres font référence (julia/r/fortran…), on peut tenter de faire
aussi bien, voire mieux."

## 0. Prior art and scope

- `priorart --repo go-scikit/refbench "multiple references julia R fortran boost"`
  (fleettools v0.108.0, 1 commit stale) exited 1 on **refbench#10** (merged).
  I read it: it is "partial runs: retries above the gate, per-row validity,
  `-perf.resume`". It matched on the word "references" and does not cover
  several references per case. Branches: clean. **No prior work on this
  subject.** refbench#10's machinery (per-row validity, `Deferred`, `Merge`,
  `CheckResume`) is what this design builds on.
- Code read: go-scikit/refbench `origin/main` 8b6268b (`case.go`, `report.go`,
  `runner.go`, `worker.go`, `pyworker.py`, README).
- go-numerics has **three code repositories**: `special`, `optimize` and
  `integrate` (`gh repo list go-numerics`). It has no fft, linalg or stats
  repository, so those are out of scope here. Each of the three already has
  a `docs/roadmap.md` that compares *features* with Julia, R and GSL (special:
  2026-11-11 inventory, `docs/roadmap.md:10-19`). This design adds *timing and
  accuracy* against the same references. It does not repeat the roadmaps'
  feature lists.

## 1. What refbench does today (the facts this design must keep)

| fact | where |
|---|---|
| One `Ref Timer` per `Case`, timed inside the reference's process | `case.go` `Case.Ref`, `Timer` |
| A round is one Go sample and one reference sample, alternating order (even: Go first) | `runner.go` `runCase` |
| Calibration doubles from `iters=1` until a run lasts `MinTime` (100 ms) | `case.go` `Calibrate` |
| Verdict is the median ratio plus a quartile-overlap test | README "Verdict"; `stats.go` `Judge` |
| `perf.json` is schema **1**, decoded with `DisallowUnknownFields` | `report.go` `SchemaVersion`, `ReadReport` |
| `Round{load_before, load_after, go_first, go_ns, ref_ns}` | `report.go` |
| `ref_versions` is a flat `map[string]string` | `report.go` `Report.RefVersions` |
| Python worker: JSON lines; data built by a **NumPy expression** (`setup`); the reference is an `eval`'d expression | `worker.go`, `pyworker.py` |
| Single-thread env: OMP/OPENBLAS/MKL/VECLIB = 1 | `worker.go` `SingleThreadEnv` |
| C references are built by `BuildC` with `-O2 -ffp-contract=off` | `cbuild.go`, README |
| CI mode needs no reference and does no timing | `rbtest`, README "CI mode" |

Three consequences for several references:

1. **The data recipe cannot be shared.** `special/bench/perf_test.go:57-67`
   builds the inputs as a NumPy string (`np.mod(np.arange(n)*0.618…, 1.0)`).
   Julia, R and Fortran cannot evaluate it. The arguments must cross
   languages as bytes (§3.4).
2. **Calibration includes the JIT.** A Julia kernel compiles on its first
   call. If compiling takes longer than `MinTime` (100 ms is common for a
   first call through a package), `Calibrate` returns `iters = 1` at once,
   and every sample becomes a single call. A worker therefore needs an
   untimed warm-up call before calibration (§3.3). That call is harmless for
   Python and C as well.
3. **`DisallowUnknownFields` makes any new field a schema break for old
   readers** (§3.6).

## 2. Reference map

The license column decides what we may do. **Source** means we may read the
code and port from it. **Black box** means we may run it, time it, and read
its documentation and papers, but never its code. Every license below was
opened, except where a row says "verify".

### 2.1 Runtimes and how to get them (pkgx, nothing installed yet)

Recipes were checked in pkgxdev/pantry and in go-zpkg/packages `recipes.txt`.
go-pkgx/packages now redirects to **go-zpkg/packages**.

| runtime | pantry project | go-zpkg `recipes.txt` | how it is built | note |
|---|---|---|---|---|
| Julia | `julialang.org` (also `julialang.org/juliaup`) | line 1216 | **vendored official binary tarball** (`warnings: vendored`, `curl … julialang-s3…/julia-$v-$TRIPLE.tar.gz`) | not built from source: see open question Q2. Latest release v1.13.3 (2026-10-26) |
| R | `r-project.org` | line 1600 | built from source; gfortran 14 runtime; **bundled reference BLAS** (`no --with-blas`) | platforms `linux` and `darwin/aarch64` only |
| GCC / gfortran | `gnu.org/gcc` (test files include `test.f90`) | line 1043 | from source | gives gfortran and g++ |
| Boost | `boost.org` | line 77 | from source; Math is header-only | the latest GitHub release is `boost-1.93.0.beta1` (2026-11-11): **pin a non-beta** |
| MPFR | `gnu.org/mpfr` | line 1071 | | oracle only |
| FLINT (contains Arb since 3.0) | `flintlib.org` | line 418 | | **LGPL-3.0** (`gh api …/flint/license`): oracle only, black box |
| GSL | `gnu.org/gsl` | line 1056 | | GPL: black box |
| FFTW, OpenBLAS, LAPACK | present (414, 1461, 1416) | | | not needed by go-numerics |

### 2.2 `special`

What each Julia function actually calls was read in SpecialFunctions.jl
v2.9.2 (MIT, `Project.toml`; the deps are OpenLibm_jll and OpenSpecFun_jll).
This decides which reference is worth timing.

| Go function(s) | reference | license | what it is | its accuracy claim (opened) |
|---|---|---|---|---|
| `Jv, Yv, Iv, Kv` (real order), `Jn, Yn, In, Kn`, `Ive, Kve` | **Bessels.jl** v0.2.8 (2023-01-28; master still active) | MIT (`gh api` SPDX) → source | pure Julia, no external dependencies | README: relative error vs ArbNumerics at 500 bits on 1000 points in (0, 100). The maxima are 2e-11 for `besselj(5,x)`, 5e-12 for `bessely(6.92,x)` and 7e-14 for `besseli(92.12,x)`. It claims **5-10× faster** than SpecialFunctions.jl: `besselj(nu,x)` 6×, `bessely` 5×, `besseli` 7×, `besselk` 4×, `besseli0`/`besselk0` 10× (Julia 1.7.2, M1 under Rosetta) |
| same | **SpecialFunctions.jl** `besselj/y/i/k(nu::Real, x)` | MIT; it wraps **AMOS** (`src/bessel.jl:234-303` ccall `zbesh_/zbesi_/zbesj_/zbesk_/zbesy_` from libopenspecfun) | AMOS | openspecfun `LICENSE.md`: AMOS is public domain through SLATEC. Faddeeva is MIT (MIT 2012) |
| `J0, J1, Y0, Y1, Jn, Yn` (integer) | SpecialFunctions.jl → **OpenLibm** `j0/j1/y0/y1/jn/yn` (`src/bessel.jl:212-219,416,440`) | OpenLibm: MIT / ISC (Moshier) / BSD-2 (msun) / FDLIBM. **Its tests `test-double.c` and `test-float.c` are LGPL: do not open them** | msun | none stated |
| Bessel J, Y, I, K (all) | **Boost.Math** `cyl_bessel_j`, `cyl_neumann`, `cyl_bessel_i`, `cyl_bessel_k` | BSL-1.0 → source | Steed's method, Temme series, A&S 9.2.19 asymptotics, Cody minimax rationals for orders 0 and 1 | Table 8.41, double, GCC 7.1 Linux. J general: max 10 ε, mean 2.24. "J0 tricky cases": 8e4 ε. GSL 2.1 and Rmath 3.2.3 on the same data are far worse (J general: 2.4e5 ε; JN: +INF) |
| same, real-argument sequence | **AMOS / SLATEC** Fortran `ZBESJ/ZBESI/ZBESK/ZBESY`, plus SLATEC real-order `DBESJ/DBESI` | public domain (SLATEC guide §4; the SLATEC TOC lists ZBES*-C and DBESJ/DBESI-D) | the routines SciPy and SpecialFunctions.jl wrap | Amos, ACM TOMS 12 (1986), the paper |
| `Gamma` | SpecialFunctions.jl `gamma(::Float64)` = **OpenLibm `tgamma`** (`src/gamma.jl:602`). **Bessels.gamma** (a re-export of Gamma.jl) | MIT | | Bessels.jl README: `Bessels.gamma` max relative error 5e-16, **5× faster** than SpecialFunctions.jl |
| `Gamma, LogGamma, Digamma, Beta` | Boost `tgamma`, `lgamma`, `digamma`, `beta` | BSL | | per-function tables in the Boost docs (only `gamma_p`/`gamma_q` and `cyl_bessel_j` were opened here) |
| `GammaP, GammaQ, GammaPInv, GammaQInv` | **Boost** `gamma_p`, `gamma_q`, `gamma_p_inv` | BSL | | Tables 8.9 and 8.10, double, GCC 7.1 Linux. `gamma_p` max 0.955 ε (medium), 0 ε elsewhere. GSL 2.1: 342 to 1.02e3 ε. Rmath 3.2.3: 389 to 1.11e3 ε. `gamma_q`: GSL "small" 1.38e10 ε |
| same | SpecialFunctions.jl `gamma_inc` (native Julia; DiDonato-Morris; the `IND` flag trades accuracy for speed, `gamma_inc.jl:849` per special's roadmap) | MIT | | |
| `BetaInc, BetaIncC, BetaIncInv, BetaIncCInv, ErfInv` | Boost `ibeta`, `ibetac`, `ibeta_inv`, `ibetac_inv`, `erf_inv` | BSL | **This is already what SciPy calls.** scipy `special/boost_special_functions.h:118-411` wraps these with `promote_double<false>` (`:18-22`) | so the current "BetaInc 2.1× slower than SciPy" row is already a comparison with Boost without promotion |
| `Erf, Erfc` | SpecialFunctions.jl → OpenLibm `erf/erfc` (`src/erf.jl:27`) | as OpenLibm | | |
| all of the above | **R** base: `gamma, lgamma, digamma, beta, besselJ/Y/I/K(…, expon.scaled)`, `pgamma, qgamma, pbeta, qbeta, pnorm, qnorm` | **GPL: black box only** | R docs: Bessel = "a translation of Fortran routines from netlib specfun" (W. J. Cody), C by M. Maechler | R docs: "warnings about accuracy loss for large arguments"; for very large orders "rarely useful". The Boost tables above measured Rmath 3.2.3 |
| same | GSL `gsl_sf_*` | **GPL: black box only** | | Boost tables above |
| oracle | mpmath 1.4.1 (stays canonical, 192/320 bits, double-double `hi+lo`; `testdata/gen/common.py:1-15`) | BSD | | |
| oracle, second opinion | MPFR (LGPL, verify), FLINT/Arb (LGPL-3.0) | **black box** | for regions where mpmath is slow (huge Bessel orders) | |

Cephes stays a source through SciPy (BSD; attribution in special `README.md`
"License").

### 2.3 `integrate`

| Go | reference | license | how timed | accuracy / work claim |
|---|---|---|---|---|
| `Quad` (QAGS/QAGI/QAWO/QAWC/QAWS) | **QUADPACK Fortran** (netlib `quadpack/readme`: Piessens, de Doncker, Ueberhuber, Kahaner), compiled with gfortran, callbacks in Fortran or C | netlib `readme` states no license. SLATEC (public domain, guide §4) includes it (TOC: QPDOC, DQAGS-D, DQAGI-D, DQK21-D), so take the **SLATEC copy** | compiled worker, §3.3 | the port claims the same subdivision path (integrate README: 116/124 fixtures). This is the purest "cost of the port" comparison: same algorithm, same integrand calls |
| `Quad` | **QuadGK.jl** `quadgk` | MIT | Julia worker, closure integrand (compiled, no interpreter in the loop) | README: returns `(integral, err)`; any order; "convergence was very rapid" on smooth integrands. Report `quadgk_count`'s evaluations as work |
| `Quad` | Boost `gauss_kronrod`, `tanh_sinh` | BSL | compiled worker | not opened: verify |
| `Quad` | R `stats::integrate` | GPL: black box | R worker. The integrand is an R closure **interpreted** per vector call. R requires `f` to be vectorised (docs) | R docs: "Based on QUADPACK routines dqags and dqagi"; `rel.tol` defaults to `eps^0.25`. The row must state that it is not a compiled-callback comparison |
| `SolveIVP` RK45/DOP853/Radau/BDF/LSODA | **OrdinaryDiffEq.jl** DP5, DP8, RadauIIA5, FBDF (Tsit5 and Vern as information) | MIT (Expat) | Julia worker, in-place RHS, `save_everystep=false`, controller set explicitly | compare nf, naccept and nreject, as integrate's roadmap already plans (`docs/roadmap.md` rows 1, 3, 8, 10) |
| `SolveIVP` DOP853/Radau | Hairer `dop853.f`, `radau5.f` | **verify**: unige.ch blocked the fetch (Q6) | compiled worker | |
| `Odeint`/LSODA | ODEPACK `dlsoda` | public domain per integrate `docs/roadmap.md` row 2: **verify on netlib** | compiled worker | nst, nfe, nje, mused |
| `SolveIVP` | R `deSolve` | GPL: black box | R worker, R closure RHS (interpreted) | |

### 2.4 `optimize`

| Go | reference | license | how timed | claim |
|---|---|---|---|---|
| `Minimize` BFGS, L-BFGS, CG, NM | **Optim.jl** | MIT | Julia worker, closures, `Optim.Options(...)` matched to SciPy's tolerances | counts in `Optim.f_calls/g_calls` |
| `Minimize` L-BFGS-B | **L-BFGS-B 3.0** (Fortran) or its C translation `stephenbeckr/L-BFGS-B-C` | the C translation is BSD-3-Clause (SPDX opened); the Fortran 3.0 license must be **verified** | compiled worker | |
| `Root`/`Fsolve`, `LeastSquares(LM)`, `Leastsq` | **MINPACK** `hybrd/hybrj/lmder/lmdif` | netlib `minpack/disclaimer`: BSD-like **with an acknowledgment clause** ("This product includes software developed by the University of Chicago, as Operator of Argonne National Laboratory"): keep the notice next to the vendored copy | compiled worker | |
| `CurveFit` | **LsqFit.jl** `curve_fit` | MIT | Julia worker | |
| `Brentq` etc. | **Roots.jl** `find_zero(…, Brent()/A42())` | MIT | Julia worker | |
| `Brentq` | Boost `tools::toms748_solve`, `brent_find_minima` | BSL | compiled worker | not opened: verify |
| `Minimize` NM/BFGS/CG/L-BFGS-B | R `stats::optim` | GPL: black box | R worker, interpreted `fn`/`gr` | R docs: Nelder-Mead, BFGS and CG from Nash (1990) Pascal via p2c; L-BFGS-B "based on Fortran code by Zhu, Byrd, Lu-Chen and Nocedal" from Netlib; `reltol = sqrt(eps)`, `maxit` 100 (500 for NM); `counts` = fn and gr calls |

For `optimize` and `integrate` a time is only meaningful together with the
**work** (function evaluations) and the **error** against the exact answer.
A faster QuadGK that uses a different number of evaluations measures the
algorithm, not the port. Both are reported (§4).

## 3. refbench extension

### 3.1 Several references per case

```go
type Ref struct {
	ID      string // stable key, "<kind>:<name>", e.g. "julia:Bessels.besseli", "boost:cyl_bessel_i", "fortran:amos.zbesi"
	Name    string // human label for the table
	License string // SPDX or "GPL-black-box"; ReadReport refuses an empty one
	Timer   Timer
	Eval    EvalFunc // nil: no accuracy column for this reference
}

type Case struct {
	// ... unchanged fields ...
	Ref  Timer // primary reference (SciPy today): unchanged semantics
	Refs []Ref // additional references; may be empty
	// Accuracy, when set, scores Go and every Ref with Eval on the same points (§4).
	Accuracy *Accuracy
}
```

- **The primary reference stays** (`Reference`, `Ref`, `ratio`, `verdict` keep
  their meaning). Existing tables, issue titles and README sentences ("vs SciPy
  1.18.1") remain true. Additional references are additional columns.
- **Round order.** With k = 1 + 1 + len(Refs) sides, round r runs the sides
  in order rotated by r mod k. Every side therefore appears in every
  position once per k rounds, and the default number of rounds is rounded up
  to a multiple of k. For k = 2 this is exactly today's alternation. (A plain
  rotation balances positions, not "who runs just before whom". For k > 2 a
  balanced Latin square would also balance that; it is left as a refinement
  because the cache effect between processes is small.)
- **Per-reference validity.** A round that ends above the gate is discarded
  for all its sides (today's rule). Retries stay per row. `Options.Refs`
  (flag `-perf.refs=julia,boost`) restricts a run to some kinds, so a long
  multi-reference row can be measured later. The deferred list carries
  `case` or `case@refID`, and `Merge` merges per (case, ref).

### 3.2 Verdict: against each reference, headline against the best

- One verdict **per reference**, with today's rule (`Judge`: quartile
  overlap, ratio Go/ref).
- A derived **`best`**: the reference with the lowest median among those
  whose accuracy was scored. The row's headline is the verdict against
  `best`.
- **An accuracy-aware label**, so that "faster than Julia but less accurate"
  is never counted as a win and "slower than a less accurate code" is not
  hidden:

  | time vs ref | max ulp, Go vs ref | label |
  |---|---|---|
  | faster or tie | Go ≤ ref | `ahead` |
  | faster or tie | Go > ref | `faster-less-accurate` (an **accuracy** defect) |
  | slower | ref ≤ Go's budget | `behind` (a **perf** defect, top priority) |
  | slower | ref > Go's budget | `tradeoff` (still a perf defect under the fleet rule, labelled so) |

- `refbench issues` drafts **one issue per case** (not one per reference),
  titled after `best`: `perf: <case> is <r>x slower than <best> on one core`.
  The body lists every reference with its ratio, verdict, max ulp, license
  and version, and carries the label above. Drafting one issue per
  reference would triple the tracker noise for a single defect.

### 3.3 Workers

One protocol for the new kinds, plain text, because R base and Fortran have
no JSON parser and adding one (jsonlite, json-fortran) is one more pinned
dependency. Python keeps its JSON protocol unchanged, so pinned repos are not
broken. The Go side hides both behind one interface.

```
rbw1 protocol, one request line → one reply line, ASCII, ≤ 4 KiB per line
L <key> <path> <cols> <rows>   load float64 LE column-major args → "ok <sha256 of the bytes read>"
W <key> <fn>                   one untimed call (JIT warm-up, page-in)  → "ok"
T <key> <fn> <iters>           time iters calls in-process            → "ns <float>"
E <key> <fn> <outpath>         evaluate once per point, write float64 → "ok <n_nonfinite> <n_raised>"
V                              versions and observed threading        → "v k=v k=v …"
error on any line              → "err <message>"; the runner stops (a series with a hole is biased)
```

- `fn` is a **name in a fixed table** compiled or defined in the worker
  (`refs/julia/kernels.jl`, `refs/r/kernels.R`, `refs/boost/kernels.cpp`).
  Nothing is eval'd from the pipe. Paths must lie inside the run directory.
  `cols*rows*8` is bounded (refuse > 256 MiB). Line length is bounded. The
  parser gets a fuzz target and a committed corpus, as `FuzzParseReply` has.
- `Calibrate` additionally discards one first call (`t(1)`, untimed). This
  fixes the JIT problem in §1 for every kind, and costs one call.

| kind | launch (all under `SingleThreadEnv()` plus the extras) | timed body | versions reported (`V`) | single-thread extras, **verified from `V`, not trusted** |
|---|---|---|---|---|
| python | unchanged (`uv run --python 3.12.12 …`) | unchanged | unchanged | unchanged |
| julia | `pkgx +julialang.org@=<pin> -- julia --startup-file=no --history-file=no -t 1 --project=<bench>/refs/julia <embedded worker.jl>` with `JULIA_DEPOT_PATH=<user cache>/refbench/julia-depot`, `JULIA_PKG_OFFLINE=true` | `out .= f.(x)` inside a function barrier on a typed `Vector{Float64}`, timed with `time_ns()`. No `@fastmath`, default `--math-mode`. `@allocated` reported as information | `VERSION`, LLVM version, `Sys.CPU_NAME`, every package in `Manifest.toml` with version and git-tree-sha1, `BLAS.get_config()`, `Threads.nthreads()`, `BLAS.get_num_threads()` | `JULIA_NUM_THREADS=1`, `JULIA_NUM_GC_THREADS=1` (Julia ≥ 1.10, docs), `-t 1`, `BLAS.set_num_threads(1)` at start. **Refuse** if `V` reports nthreads ≠ 1 |
| r | `pkgx +r-project.org@=<pin> -- Rscript --vanilla <embedded worker.R>` | one vectorised call (`besselI(x, nu, expon.scaled=…)`, `pgamma(x, a)`) per iteration; R allocates its result. Clock: `Sys.time()` differences over ≥ 100 ms samples | `R.version.string`, `La_library()`, `extSoftVersion()` | bundled reference BLAS (pkgx build: no `--with-blas`); OMP=1 already set |
| compiled (C, C++, Fortran) | binary built by `BuildWorker` (a generalised `BuildC`): pkgx-pinned gcc/g++/gfortran and pinned Boost headers; sources committed with SHA-256 | a C/Fortran loop over the array writing `out[i]`, with a checksum returned so the loop cannot be removed | compiler `__VERSION__`, flags, `BOOST_VERSION`, Boost policy, `LDBL_MANT_DIG`, the SHA-256 of every source | no OpenMP, no threads |

**Compiler flags of compiled references.** For speed the fair target is the
reference's normal release build, not our parity build. The default is
`-O2`, with FP contraction at the compiler default and no `-march=native`.
The flags are recorded, and a different flag set is a different `Ref.ID`.
`NoFMAFlags` stays for parity rows only.

**Boost policy.** Boost's default is `promote_double<true>`, which computes
`double` in `long double` (Boost docs, "Internal Floating-point Promotion
Policies"). SciPy turns it off. On arm64 macOS `long double` is `double`, so
the two policies are the same build. On x86-64 promotion means 80-bit x87.
On aarch64 Linux it means 128-bit software quad, which would be very slow.
So the design registers `boost` (promote off, SciPy's choice, the speed
reference) and, on x86-64 only, `boost-promote` (the accuracy reference that
gives Boost's 0 ε tables). `LDBL_MANT_DIG` goes in the record.

**Where worker sources live.** These include the embedded `worker.jl`,
`worker.R` and the rbw1 C/Fortran harness, which are generic. They also
include the per-library kernels: Boost special functions, AMOS, QUADPACK,
MINPACK and L-BFGS-B. The generic part goes in refbench, embedded like
`pyworker.py`. The library kernels go in **one shared place, not per
repository** (Q1). Only permissive sources are ever vendored. GPL references
are reached only through their installed binaries (R) or their documented
API (GSL, if ever).

### 3.4 Data crosses languages as bytes

- The Go side writes each case's argument arrays once per run, as float64
  little-endian column-major, into
  `os.UserCacheDir()/refbench/<repo>/<commit>/<key>.f64`. That directory
  belongs to the tool, not to the repository and not to `/tmp`.
- `L` returns the SHA-256 of the bytes the worker read. The runner compares
  it with the Go side's digest before any timing, and refuses on a mismatch,
  naming both digests. This replaces `DataHash` for the new kinds and
  generalises `TestDataParity`.
- The Python worker gains the same `load` request (read the file with
  `np.fromfile`), so all sides time exactly the same bytes. The NumPy
  `setup` expression stays for compatibility.

### 3.5 Pinned versions in the record

- `ref_versions` stays a flat map with **namespaced keys**:
  `julia/julia=1.13.3`, `julia/Bessels=0.2.8@<tree-sha1>`,
  `boost/version=1.92.0`, `boost/policy=promote_double:false`,
  `fortran/gfortran=…`, `fortran/src/amos=sha256:…`, `r/R=…`. This is no
  schema change. `CheckResume` already refuses differing reference
  versions, so it now covers every kind.
- Pins live in the repository: `bench/refs.txt` (uv, hashes; unchanged),
  `bench/refs/julia/Project.toml` and `Manifest.toml` (exact versions and
  git-tree-sha1 content hashes, Julia's equivalent of `--generate-hashes`),
  `bench/refs/toolchain.txt` (`julialang.org@=1.13.3`, `r-project.org@=…`,
  `gnu.org/gcc@=…`, `boost.org@=…`).
- `refbench prepare` (new subcommand) instantiates and precompiles the Julia
  project and builds the compiled workers, **outside** any gated run.
  Precompilation is minutes of CPU and must not happen inside a timed
  window, nor push the load over the gate during a run.

### 3.6 JSON compatibility: exactly what breaks

New fields:

- On `Row`: `refs: [{id, name, license, iters, summary, ratio, verdict, label, accuracy}]`,
  `go_accuracy`, `best`.
- On `Round`: `refs_ns: {id: ns}` and `order: [side ids]`.

`Round.ref_ns`, `Row.ratio` and `Row.verdict` keep the primary reference's
meaning.

- **A repo with no `Refs` keeps writing schema 1, byte for byte.** Nothing
  changes for repositories that do not opt in.
- **A repo with `Refs` writes schema 2.** The new `ReadReport` reads 1 and 2
  (1 → no extra refs).
- **Break:** an older refbench (any build with `SchemaVersion = 1` and
  `DisallowUnknownFields`) refuses a schema-2 file with
  `perf.json: schema 2, this build reads 1`. That covers the `rbtest` in a
  repo's `bench/go.mod` and the installed `refbench table/check/issues`. A
  repository that adds `Refs` must bump refbench in the same PR. The refusal
  is loud, never silent.
- The table gains a second section, "Against other references" (§4). The
  first section is unchanged, so `refbench check` on a schema-1 file is
  unchanged.

### 3.7 Staying cheap in CI

- CI mode still starts no runtime and times nothing. It additionally checks
  four things:
  - every `Ref.ID` declared in the registry has a row entry, or is in
    `deferred`;
  - every ref has a non-empty `license`;
  - no ref is labelled GPL unless its kind is a black-box kind (`r`, `gsl`);
  - the table regenerates from the JSON.
- refbench's own tests use fake workers through the replaceable
  `execCommand` (today's pattern). Coverage stays at 100 %. Fuzz targets for
  the rbw1 reply parser and the schema-2 reader are added.
- An optional, manually dispatched lane (weekly at most) installs Julia and
  R through pkgx and runs only the protocol smoke test (`V`, `L`, `W`, one
  `E`). It never times.

## 4. Accuracy in the same rows

The fleet contract is "a stated bound against an extended-precision oracle".
The same oracle must judge every reference. Judge and subject must count
alike: the same points, the same ulp function, the same treatment of
non-finite values.

```go
type Accuracy struct {
	Points [][]float64                   // columns; written once, loaded with L
	Score  func(got []float64) AccResult // the repo's own scorer (special: ulp vs mpmath hi+lo)
}
type AccResult struct {
	MaxULP     float64 // +Inf if a finite value was expected and NaN/Inf returned
	MaxRegion  string  // where the max occurs
	MaxULPFin  float64 // max over points where the reference returned a finite value
	Coverage   float64 // fraction of points the reference evaluated (exceptions → NaN, counted)
	Points     int
}
```

- **special.** `Points` are the fixture arguments of the function. The
  fixtures are `testdata/fixtures/<f>.tsv.gz` with columns region, args,
  mpmath hi, mpmath lo, scipy (`testdata/gen/common.py:146`). `Score` is the
  existing ulp measure of `TestAccuracyBound`. Go's own column comes from
  the same `Score` applied to the Go function on the same points.
- **The first run must reproduce the fixtures' SciPy column bit for bit**
  through the Python worker's `E`. That is the negative control that the
  eval path passes the arguments unaltered (a planted perturbation of one
  argument must make it fail).
- **Domain conventions differ.** Bessels.jl throws `DomainError` for a
  negative x with a complex result (its README), R warns and returns NaN, and
  Boost raises according to its policy. Workers catch per point, return NaN
  and count it (`n_raised`). Hence the two columns `MaxULP` (honest
  worst case) and `MaxULPFin` with `Coverage`, so that a reference which
  refuses half the domain does not look accurate.
- **optimize and integrate.** `E` returns per problem `(value, nfev[, njev])`.
  `Score` gives the error against the exact optimum or integral (the
  existing `math/big` and mpmath oracles in `accuracy_bound_test.go` and
  integrate's fixtures). The table shows time, error and evaluations side by
  side.
- Accuracy is deterministic, so it is computed **once per (case, ref,
  versions, machine)**, ungated, before the timing rounds, and stored in the
  record. `-perf.resume` does not recompute it unless the versions changed.
- Table, second section:

  `| case | Go ns | Go max ulp | best | ratio vs best | label | julia:… ratio (max ulp, cov) | boost:… | r:… |`

## 5. Priorities

The expected value is the likely gap, times how often the function is used,
times whether the reference is also a legal algorithm source. The cost
assumes the core (§3.1-3.6) and **compiled workers** first: gfortran is
present and Boost is header-only. Then the **Julia worker**, then **R**.

| # | comparison | reference (license) | why | expected | cost |
|---|---|---|---|---|---|
| 1 | `Iv, In, Ive, Kv, Kn, Kve` | **Bessels.jl** (MIT, source) + Boost `cyl_bessel_i/k` (BSL) | Go is already 8.0× (Iv), 5.3× (In), 2.3× (Kv), 1.88× (Kn), 1.71× (Ive) slower than SciPy (special README "Speed"). SpecialFunctions.jl routes these to AMOS (`bessel.jl:251,285`), and Bessels.jl claims 7× (`besseli`) and 4× (`besselk`) over it, with max relative error ≤ 7e-14 for `besseli`. **The gap against Julia may be 15-50×**, and Bessels.jl is MIT, so the algorithms can be ported | very high | Julia worker (M) |
| 2 | `Jv, Yv, Jn, Yn` | **Bessels.jl** (MIT) + Boost `cyl_bessel_j/cyl_neumann` (BSL) | Yn is 33.4× slower than SciPy, Jn 1.52×, Jv 1.28×, Yv 1.22×. Bessels.jl claims 6× and 5× over AMOS but up to 2e-11 relative near zeros, where Go holds 0.5-1.7 ulp: likely a `tradeoff`, which is exactly what the accuracy column is for. Boost reaches 10 ε max on J general (Table 8.41) | very high | shares #1 |
| 3 | `Gamma, LogGamma, Digamma, Beta` | Bessels.gamma / Gamma.jl (MIT), OpenLibm `tgamma` via SpecialFunctions.jl, Boost `tgamma/lgamma/digamma` (BSL, both policies on x86-64) | Go beats SciPy (Gamma 0.83, 8.14 ns/element), but Bessels.jl claims `gamma` is 5× faster than OpenLibm's `tgamma` at 5e-16. Gamma is the most-called kernel of the stack (every distribution) | high | cheap once #1 exists; Boost via compiled worker (S) |
| 4 | `GammaP, GammaQ, GammaPInv, GammaQInv` | **Boost** `gamma_p/gamma_q/gamma_p_inv` (BSL); SpecialFunctions.jl `gamma_inc` (MIT) | Go is 1.15× (P/Q) and 1.5× (inverses) slower than SciPy. Boost tables: ≤ 0.955 ε against GSL/Rmath at 10²-10³ ε. SciPy routes only `gamma_p_inva`, `gamma_q_inva`, `lgamma_p` and `lgamma_q` to Boost (`boost_special_functions.h:2566-2757`), so Boost `gamma_p` is a **new** data point | high | compiled worker (S) |
| 5 | `Quad` (QAGS, QAGI, weighted) | **QUADPACK Fortran** from SLATEC (public domain), gfortran `-O2`, Fortran or C callbacks; then QuadGK.jl (MIT) | The README says 0.08-0.29 of SciPy's time even with compiled callbacks. The same Fortran, compiled, is the honest test of what a pure-Go port costs (same path, same evaluations). It also checks that claim | medium-high (validates or corrects a published claim) | compiled worker (S) |
| 6 | `Minimize` BFGS / L-BFGS-B, `Brentq` | **Optim.jl** (MIT), L-BFGS-B-C (BSD-3), Roots.jl (MIT) | "330× faster than SciPy" on Rosenbrock n=2 is mostly Python callback overhead (optimize README says so). Compiled-callback references give the number that will survive scrutiny | medium-high | Julia worker (shared) + compiled (S) |
| 7 | `Root`, `LeastSquares(LM)`, `Leastsq`, `CurveFit` | **MINPACK** (BSD-like + acknowledgment) gfortran; LsqFit.jl (MIT) | the Go MINPACK port with FMA/go-simd above 32 residuals against the original Fortran: a pure kernel-speed comparison | medium | compiled (S) |
| 8 | `SolveIVP` RK45/DOP853/Radau/BDF | **OrdinaryDiffEq.jl** DP5/DP8/RadauIIA5/FBDF (MIT); Hairer Fortran (license Q6) | Julia's ODE suite is the domain's performance reference. Counts (nf, steps) are already on integrate's roadmap, so the timings complete that picture | medium | Julia (OrdinaryDiffEq precompile is heavy: `refbench prepare`) (M) |

Below the top 8:

- **BetaInc/BetaIncInv/ErfInv vs Boost.** SciPy already *is* Boost without
  promotion (§2.2), so only `boost-promote` (x86-64) adds information, as
  accuracy.
- **Erf/Erfc vs OpenLibm** (through Julia).
- **AMOS compiled directly**: redundant with SpecialFunctions.jl, and cheap
  once the Fortran harness exists.
- **R** for all functions, black box: low speed value (Cody/Nash code, and
  interpreted callbacks for integrate and optim), but it is "what statisticians
  run". It is mostly useful for go-statistics later.
- **GSL**, black box: Boost's tables already show its accuracy.

Order of work:

1. Core: Refs, rounds, schema 2, `Accuracy`, rbw1, `Calibrate` warm-up, byte data.
2. Compiled worker, with Boost (#3, #4) and QUADPACK/MINPACK (#5, #7).
3. Julia worker (#1, #2, #6, #8).
4. R worker.

Each step ships with its README, landing and docs updates, per the fleet
rule.

## 6. Open questions for the user

- **Q1. Home of the per-library kernels** (Boost, AMOS, QUADPACK, MINPACK,
  L-BFGS-B). The options are inside refbench (`workers/`), a new shared repo
  (`go-scikit/refworkers`), or each repo's `bench/refs/`. Recommendation: a
  shared repo, so there is one implementation for every consumer.
- **Q2. Julia comes from pkgx as a vendored upstream binary** (`julialang.org`
  `package.yml`: `warnings: vendored`), not built by go-zpkg. Is that
  acceptable under the sovereign-build doctrine for a *measurement*
  reference, or must go-zpkg build Julia from source first?
- **Q3. GSL and Rmath as black boxes.** Is it acceptable to write our own
  worker that links them, if we never read their code or headers, only the
  manuals, and never distribute the binary? Or is it R through `Rscript`
  only?
- **Q4. Issue policy.** Should a case slower than a *less accurate*
  reference (`tradeoff`) be drafted as a perf defect? The fleet rule says
  yes. The design drafts it with the label.
- **Q5. Boost promotion.** Should `boost-promote` be measured on the x86-64
  host only? On the reference host (Apple M4 Max) the two policies are the
  same build.
- **Q6. Licences not settled by an opened document:**
  - Hairer DOP853/RADAU5: the site blocked the fetch;
  - L-BFGS-B 3.0 Fortran (the C translation is BSD-3);
  - ODEPACK;
  - Cody's netlib SPECFUN (the code behind R's Bessel functions, which would
    give R's algorithm without GPL);
  - MPFR.
- **Q7. Using Bessels.jl (MIT) as an algorithm source** to close gaps #1 and
  #2. Measuring it is this design. Porting it is the follow-up the
  measurement will probably call for.
- **Q8. Primary reference.** Recommendation: keep SciPy as the primary, so
  that existing tables and READMEs stay true. Alternatively make the primary
  the best reference.

## 7. Sources opened for this design

- go-scikit/refbench `origin/main` 8b6268b: `README.md`, `case.go`, `report.go`, `runner.go`, `worker.go`, `pyworker.py`; PR #10 body.
- go-numerics/special README; `docs/roadmap.md`; local clone `go-numerics-special-ynjn` 8255bbe: `bench/perf_test.go:1-90`, `testdata/gen/common.py:1-40,146`.
- go-numerics/optimize README and `docs/roadmap.md` (headings and rows); go-numerics/integrate README and `docs/roadmap.md` (headings and rows).
- pkgxdev/pantry `projects/julialang.org/package.yml`, `projects/r-project.org/package.yml`, `projects/boost.org/package.yml`, directory listings of `gnu.org/gcc`, `gnu.org/mpfr`, `fftw.org`, `flintlib.org`; go-zpkg/packages `recipes.txt`.
- JuliaMath/SpecialFunctions.jl `LICENSE`, `Project.toml` (v2.9.2), `src/bessel.jl`, `src/erf.jl`, `src/gamma.jl` (ccall lines); JuliaMath/openspecfun `LICENSE.md`, `README.md`; JuliaMath/openlibm `LICENSE.md`; JuliaMath/Bessels.jl README, license, latest release; JuliaMath/QuadGK.jl README and license; JuliaNLSolvers/Optim.jl `LICENSE.md`; JuliaNLSolvers/LsqFit.jl `LICENSE.md`; SciML/OrdinaryDiffEq.jl `LICENSE.md`; JuliaMath/Roots.jl license (SPDX); flintlib/flint license (SPDX); stephenbeckr/L-BFGS-B-C license (SPDX); boostorg/math license (SPDX).
- scipy/scipy `scipy/special/boost_special_functions.h` (policy at :18-32, wrapped functions).
- Boost.Math docs: Bessel first/second kind (Table 8.41), incomplete gamma (Tables 8.9, 8.10), "Internal Floating-point Promotion Policies", "Relative error".
- Julia manual, "Environment Variables" (`JULIA_NUM_THREADS`, `JULIA_NUM_GC_THREADS`, `JULIA_EXCLUSIVE`, `JULIA_CPU_TARGET`, `JULIA_DEPOT_PATH`, `JULIA_PKG_OFFLINE`).
- R manual pages: `base::Bessel`, `stats::integrate`, `stats::optim`.
- netlib: `minpack/disclaimer`, `quadpack/readme`, `slatec/guide` (§4), `slatec/toc` (first 100 kB).
- Not opened (claims above marked "verify"): Hairer's site (blocked), the Boost quadrature and root-finding docs, the MPFR licence, the ODEPACK licence, the L-BFGS-B 3.0 Fortran licence, BenchmarkTools.jl (not used: refbench's own calibration and statistics stay the single source of truth).

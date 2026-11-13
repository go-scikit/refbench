package refbench

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

func TestParseRBW1Reply(t *testing.T) {
	ok := map[string]RBW1Reply{
		"ok":                            {Kind: "ok"},
		"ok abc":                        {Kind: "ok", Args: []string{"abc"}},
		"ok 1 2":                        {Kind: "ok", Args: []string{"1", "2"}},
		"ns 12.5":                       {Kind: "ns", NS: 12.5},
		"ns 0":                          {Kind: "ns"},
		"v julia=1.13.3 a/b+c=x@y":      {Kind: "v", Versions: map[string]string{"julia": "1.13.3", "a/b+c": "x@y"}},
		"v threads=1":                   {Kind: "v", Versions: map[string]string{"threads": "1"}},
		"ok " + strings.Repeat("a", 64): {Kind: "ok", Args: []string{strings.Repeat("a", 64)}},
	}
	for in, want := range ok {
		got, err := ParseRBW1Reply([]byte(in))
		if err != nil || got.Kind != want.Kind || got.NS != want.NS || len(got.Args) != len(want.Args) || len(got.Versions) != len(want.Versions) {
			t.Errorf("%q = %+v, %v; want %+v", in, got, err, want)
		}
		for k, v := range want.Versions {
			if got.Versions[k] != v {
				t.Errorf("%q: %s = %q", in, k, got.Versions[k])
			}
		}
	}
	bad := []string{
		``, `err boom`, `err`, `ok  1`, `ok 1 `, ` ok`, `ns`, `ns 1 2`, `ns -1`, `ns nan`, `ns +Inf`, `ns 1e999`, `ns x`,
		`v`, `v a`, `v =x`, `v a=`, `v a=1 a=2`, `v bad key=1`, `v a b=1`, `nope 1`, "ok \x00", "ok café", "ok \t1", "ok \x7f",
		"v " + strings.Repeat("k=1 ", 300),
	}
	for _, in := range bad {
		if _, err := ParseRBW1Reply([]byte(in)); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	if _, err := ParseRBW1Reply([]byte("err boom")); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err reply: %v", err)
	}
	if _, err := ParseRBW1Reply([]byte("ok " + strings.Repeat("x", RBW1MaxLine))); !errors.Is(err, ErrRBW1LineTooLong) {
		t.Errorf("oversized: %v", err)
	}
	if _, err := ParseRBW1Reply([]byte("ok " + strings.Repeat("x", RBW1MaxLine-3))); err != nil {
		t.Errorf("a line of exactly 4096 bytes refused: %v", err)
	}
}

func FuzzParseRBW1Reply(f *testing.F) {
	for _, s := range []string{"ok", "ok " + strings.Repeat("0", 64), "ns 12.5", "ns 1e999", "v julia=1.13.3 threads=1", "err boom", "ok 1 2", "v a=1 a=2", "ok  x", "ns -0"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseRBW1Reply(b)
		if err != nil {
			return
		}
		if len(b) > RBW1MaxLine || !printableASCII(b) {
			t.Fatalf("accepted %q", b)
		}
		if r.Kind == "ns" && !finiteNonNeg(r.NS) {
			t.Fatalf("accepted ns %v", r.NS)
		}
		if r.Kind == "v" && len(r.Versions) > rbw1MaxVersions {
			t.Fatal("too many versions")
		}
		if len(r.Args) > 0 && strings.Contains(strings.Join(r.Args, ""), " ") {
			t.Fatal("space in a token")
		}
	})
}

// fake is an in-process worker: respond maps a request line to a reply line.
func fake(t *testing.T, dir string, funcs []string, respond func(line string) string) *RBW1 {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		sc := bufio.NewScanner(inR)
		for sc.Scan() {
			fmt.Fprintln(outW, respond(sc.Text()))
		}
		outW.Close()
	}()
	w, err := NewRBW1(inW, outR, RBW1Config{Dir: dir, Funcs: funcs})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inW.Close(); outR.Close() })
	return w
}

func TestRBW1Requests(t *testing.T) {
	dir := t.TempDir()
	path, rows, sha, err := WriteF64(dir, "x", []float64{1, 2, 3}, []float64{4, 5, 6})
	if err != nil || rows != 3 {
		t.Fatal(err)
	}
	var calls []string
	w := fake(t, dir, []string{"gamma", "k.v2"}, func(line string) string {
		calls = append(calls, line)
		f := strings.Fields(line)
		switch f[0] {
		case "L":
			return "ok " + sha
		case "W":
			return "ok"
		case "T":
			return "ns " + f[3] + "000"
		case "E":
			os.WriteFile(f[3], make([]byte, 24), 0o600)
			return "ok 1 2"
		case "V":
			return "v threads=1"
		}
		return "err unknown"
	})
	if err := w.Load("x", path, 2, 3, sha); err != nil {
		t.Fatal(err)
	}
	tm := w.Timer("x", "gamma")
	d, err := tm(5)
	if err != nil || d != 5000 {
		t.Fatalf("%v %v", d, err)
	}
	if _, err := tm(1); err != nil {
		t.Fatal(err)
	}
	warmups := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "W ") {
			warmups++
		}
	}
	if warmups != 1 {
		t.Errorf("%d warm-ups, want one before the first timed call: %v", warmups, calls)
	}
	out := filepath.Join(dir, "out.f64")
	nf, raised, err := w.Eval("x", "gamma", out)
	if err != nil || nf != 1 || raised != 2 {
		t.Errorf("eval = %d %d %v", nf, raised, err)
	}
	var seen [2]int
	vals, err := w.Evaler("x", "k.v2", out, func(a, b int) { seen = [2]int{a, b} })()
	if err != nil || len(vals) != 3 || seen != [2]int{1, 2} {
		t.Errorf("evaler = %v %v %v", vals, seen, err)
	}
	if v, err := w.Versions(); err != nil || v["threads"] != "1" {
		t.Errorf("versions = %v %v", v, err)
	}
}

func TestRBW1RefusesBeforeSending(t *testing.T) {
	dir := t.TempDir()
	sent := 0
	w := fake(t, dir, []string{"f"}, func(string) string { sent++; return "ok" })
	sha := strings.Repeat("0", 64)
	for name, err := range map[string]error{
		"fn not in table":   w.Warm("x", "g"),
		"fn injection":      w.Warm("x", "f; rm -rf /"),
		"bad key":           w.Warm("x y", "f"),
		"relative path":     w.Load("x", "rel.f64", 1, 1, sha),
		"outside":           w.Load("x", filepath.Join(filepath.Dir(dir), "o.f64"), 1, 1, sha),
		"dotdot":            w.Load("x", dir+"/../o.f64", 1, 1, sha),
		"the dir itself":    w.Load("x", dir, 1, 1, sha),
		"space in path":     w.Load("x", dir+"/a b", 1, 1, sha),
		"zero cols":         w.Load("x", filepath.Join(dir, "a"), 0, 1, sha),
		"huge":              w.Load("x", filepath.Join(dir, "a"), 1<<20, 1<<20, sha),
		"overflow 32 bits":  w.Load("x", filepath.Join(dir, "a"), 1<<28, 1<<28, sha),
		"bad load key":      w.Load("", filepath.Join(dir, "a"), 1, 1, sha),
		"eval outside":      evalErr(w, "/etc/passwd"),
		"time bad fn":       timeErr(w, "g", 1),
		"time zero iters":   timeErr(w, "f", 0),
		"eval bad fn":       evalFnErr(w, dir),
		"unloaded evaler":   evalerErr(w, dir),
		"warm in timer":     timerErr(w, "g"),
		"huge request line": w.Load("x", filepath.Join(dir, strings.Repeat("a", RBW1MaxLine)), 1, 1, sha),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if sent != 0 {
		t.Errorf("%d requests reached the worker", sent)
	}
}

func evalErr(w *RBW1, p string) error { _, _, err := w.Eval("x", "f", p); return err }
func timeErr(w *RBW1, fn string, it int) error {
	_, err := w.Time("x", fn, it)
	return err
}
func evalFnErr(w *RBW1, dir string) error {
	_, _, err := w.Eval("x", "zz", filepath.Join(dir, "o"))
	return err
}
func evalerErr(w *RBW1, dir string) error {
	_, err := w.Evaler("never", "f", filepath.Join(dir, "o"), nil)()
	return err
}
func timerErr(w *RBW1, fn string) error { _, err := w.Timer("x", fn)(1); return err }

func TestRBW1RefusesBadReplies(t *testing.T) {
	dir := t.TempDir()
	sha := strings.Repeat("0", 64)
	reply := ""
	w := fake(t, dir, []string{"f"}, func(string) string { return reply })
	check := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: accepted %q", name, reply)
		}
	}
	for _, r := range []string{"ns 1", "ok", "ok zz", "ok " + strings.Repeat("A", 64), "ok " + strings.Repeat("1", 64), "err no"} {
		reply = r
		check("load", w.Load("x", filepath.Join(dir, "a"), 1, 1, sha))
	}
	for _, r := range []string{"ns 1", "ok 1", "err no", "v a=1"} {
		reply = r
		check("warm", w.Warm("x", "f"))
	}
	for _, r := range []string{"ok", "v a=1", "ns 1e300", "err x"} {
		reply = r
		_, err := w.Time("x", "f", 1)
		check("time", err)
	}
	for _, r := range []string{"ns 1", "ok", "ok 1", "ok -1 0", "ok 0 x", "ok 1 2 3", "err x"} {
		reply = r
		_, _, err := w.Eval("x", "f", filepath.Join(dir, "o"))
		check("eval", err)
	}
	for _, r := range []string{"ok", "ns 1", "err x"} {
		reply = r
		_, err := w.Versions()
		check("versions", err)
	}
}

func TestRBW1LoadDetectsAnAlteredInput(t *testing.T) {
	// Negative control: a worker that perturbs one value of its input reads
	// another digest, and Load refuses it, naming both.
	dir := t.TempDir()
	path, _, sha, err := WriteF64(dir, "x", []float64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	b[8] ^= 1 // one bit of the second value
	altered := filepath.Join(dir, "y.f64")
	os.WriteFile(altered, b, 0o600)
	_, _, shaAltered, _ := WriteF64(dir, "z", []float64{1, math.Float64frombits(math.Float64bits(2) ^ 1), 3})
	w := fake(t, dir, []string{"f"}, func(string) string { return "ok " + shaAltered })
	err = w.Load("x", path, 1, 3, sha)
	if err == nil || !strings.Contains(err.Error(), sha) || !strings.Contains(err.Error(), shaAltered) {
		t.Errorf("err = %v", err)
	}
}

func TestRBW1ProcessEnded(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { io.Copy(io.Discard, inR); outW.Close() }()
	w, err := NewRBW1(inW, outR, RBW1Config{Dir: t.TempDir(), Funcs: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	inW.Close() // the write fails
	if err := w.Warm("x", "f"); err == nil {
		t.Error("write to a closed worker accepted")
	}
	if err := w.Close(); err != nil {
		t.Error(err)
	}
	// EOF on the reply stream.
	in2R, in2W := io.Pipe()
	out2R, out2W := io.Pipe()
	go func() { io.Copy(io.Discard, in2R) }()
	out2W.Close()
	w2, _ := NewRBW1(in2W, out2R, RBW1Config{Dir: t.TempDir(), Funcs: []string{"f"}})
	if err := w2.Warm("x", "f"); err == nil || !strings.Contains(err.Error(), "ended") {
		t.Errorf("eof: %v", err)
	}
	// A reader that fails.
	in3R, in3W := io.Pipe()
	go func() { io.Copy(io.Discard, in3R) }()
	w3, _ := NewRBW1(in3W, errReader{}, RBW1Config{Dir: t.TempDir(), Funcs: []string{"f"}})
	if err := w3.Warm("x", "f"); err == nil || !strings.Contains(err.Error(), "io") {
		t.Errorf("read error: %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("io") }

func TestNewRBW1Refuses(t *testing.T) {
	good := t.TempDir()
	for name, cfg := range map[string]RBW1Config{
		"relative":  {Dir: "rel", Funcs: []string{"f"}},
		"unclean":   {Dir: good + "/./x", Funcs: []string{"f"}},
		"space":     {Dir: good + "/a b", Funcs: []string{"f"}},
		"no funcs":  {Dir: good},
		"bad func":  {Dir: good, Funcs: []string{"1f"}},
		"func a b":  {Dir: good, Funcs: []string{"a b"}},
		"non ascii": {Dir: good + "/é", Funcs: []string{"f"}},
	} {
		if _, err := NewRBW1(nopCloser{}, strings.NewReader(""), cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

type nopCloser struct{}

func (nopCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopCloser) Close() error                { return nil }

func TestBombOversizedLine(t *testing.T) {
	// A worker that streams 256 MiB without a newline must be refused after
	// 4 KiB, under a memory limit, without the heap growing.
	old := debug.SetMemoryLimit(128 << 20)
	defer debug.SetMemoryLimit(old)
	outR, outW := io.Pipe()
	go func() {
		chunk := []byte(strings.Repeat("x", 1<<20))
		for i := 0; i < 256; i++ {
			if _, err := outW.Write(chunk); err != nil {
				return
			}
		}
		outW.Close()
	}()
	w, err := NewRBW1(nopCloser{}, outR, RBW1Config{Dir: t.TempDir(), Funcs: []string{"f"}})
	if err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err = w.Warm("x", "f")
	outR.Close()
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrRBW1LineTooLong) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > 8<<20 {
		t.Errorf("heap grew by %d bytes", grown)
	}
	// And a line of 1 MiB handed to the parser.
	if _, err := ParseRBW1Reply([]byte(strings.Repeat("x", 1<<20))); !errors.Is(err, ErrRBW1LineTooLong) {
		t.Errorf("parser: %v", err)
	}
}

// TestHelperRBW1 is the fake rbw1 worker process the tests below start.
func TestHelperRBW1(t *testing.T) {
	mode := os.Getenv("REFBENCH_RBW1_HELPER")
	if mode == "" {
		return
	}
	if mode == "bomb" {
		// 64 MiB without a newline, under the memory limit the test sets.
		chunk := []byte(strings.Repeat("x", 1<<20))
		for i := 0; i < 64; i++ {
			if _, err := os.Stdout.Write(chunk); err != nil {
				os.Exit(0)
			}
		}
		os.Exit(0)
	}
	threads, julia := "1", "1.13.3"
	switch mode {
	case "threads":
		threads = "4"
	case "wrongjulia":
		julia = "1.12.0"
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		switch {
		case mode == "die":
			os.Exit(0)
		case mode == "noversion":
			fmt.Println("ok")
		case sc.Text() == "V":
			fmt.Printf("v julia=%s threads=%s gc_threads=1 blas_threads=1 Bessels=0.2.8@abc dir=%s\n", julia, threads, os.Getenv("RBW1_DIR"))
		default:
			fmt.Println("ok")
		}
	}
	os.Exit(0)
}

func helperRBW1(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("REFBENCH_RBW1_HELPER", mode)
	old := execCommand
	t.Cleanup(func() { execCommand = old })
	execCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperRBW1")
	}
}

func juliaCfg(t *testing.T) JuliaConfig {
	return JuliaConfig{Pkgx: "pkgx", Version: "1.13.3", Project: "/p", Kernels: "/k.jl", Depot: "/d", Dir: t.TempDir(), Funcs: []string{"gamma"}}
}

func TestStartJulia(t *testing.T) {
	helperRBW1(t, "ok")
	c := juliaCfg(t)
	w, v, err := StartJulia(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if v["julia/julia"] != "1.13.3" || v["julia/Bessels"] != "0.2.8@abc" || v["julia/dir"] != c.Dir {
		t.Errorf("versions = %v", v)
	}
	if err := w.Warm("x", "gamma"); err != nil {
		t.Error(err)
	}
}

func TestStartJuliaRefuses(t *testing.T) {
	for mode, want := range map[string]string{"threads": "threads=4", "wrongjulia": "pin", "noversion": "want v", "die": "ended"} {
		helperRBW1(t, mode)
		_, _, err := StartJulia(context.Background(), juliaCfg(t))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", mode, err)
		}
	}
	// A bad configuration is refused before a process starts.
	c := juliaCfg(t)
	c.Dir = "rel"
	if _, _, err := StartJulia(context.Background(), c); err == nil {
		t.Error("relative dir accepted")
	}
	// And a process that cannot start.
	old := execCommand
	defer func() { execCommand = old }()
	execCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/nonexistent/refbench-test")
	}
	if _, _, err := StartJulia(context.Background(), juliaCfg(t)); err == nil {
		t.Error("missing executable accepted")
	}
}

func TestBombOversizedLineFromAProcess(t *testing.T) {
	// The process writes 64 MiB without a newline; the child runs under a
	// GOMEMLIMIT of its own and the parent under the limit of the previous test.
	helperRBW1(t, "bomb")
	t.Setenv("GOMEMLIMIT", "64MiB")
	old := debug.SetMemoryLimit(128 << 20)
	defer debug.SetMemoryLimit(old)
	w, err := StartRBW1(context.Background(), RBW1Config{Dir: t.TempDir(), Funcs: []string{"f"}}, os.Environ(), "x")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Warm("x", "f") }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRBW1LineTooLong) {
			t.Errorf("err = %v, want a refusal", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no refusal")
	}
	w.cmd.Process.Kill()
	_ = w.Close()
}

func TestJuliaArgsAndEnv(t *testing.T) {
	c := juliaCfg(t)
	args := JuliaArgs(c)
	want := []string{"+julialang.org@1.13.3", "--", "julia", "--startup-file=no", "--history-file=no", "-t", "1", "--project=/p", "-e"}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("args[%d] = %q, want %q", i, args[i], w)
		}
	}
	if args[len(want)] != JuliaWorkerSource || args[len(args)-1] != "/k.jl" || !strings.Contains(JuliaWorkerSource, "RBW1_DIR") {
		t.Error("the embedded worker or the kernels file is not passed")
	}
	env := strings.Join(JuliaEnv(c), "\n")
	for _, e := range []string{"JULIA_NUM_THREADS=1", "JULIA_NUM_GC_THREADS=1", "JULIA_DEPOT_PATH=/d", "JULIA_PKG_OFFLINE=true", "OMP_NUM_THREADS=1"} {
		if !strings.Contains(env, e) {
			t.Errorf("env lacks %s", e)
		}
	}
	if !strings.Contains(CompiledHarnessSource, "rbw1_kernels") {
		t.Error("harness not embedded")
	}
}

func TestCheckSingleThreadAndNamespace(t *testing.T) {
	good := map[string]string{"threads": "1", "gc_threads": "1", "blas_threads": "1", "julia": "1.13.3"}
	if err := CheckSingleThread(good); err != nil {
		t.Error(err)
	}
	for name, v := range map[string]map[string]string{
		"missing": {"julia": "1"},
		"threads": {"threads": "2"},
		"gc":      {"threads": "1", "gc_threads": "4"},
		"blas":    {"threads": "1", "blas_threads": "8"},
		"empty":   {},
	} {
		if CheckSingleThread(v) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	ns := NamespaceVersions("julia", good)
	if ns["julia/julia"] != "1.13.3" || len(ns) != len(good) {
		t.Errorf("ns = %v", ns)
	}
}

func TestDataDirAndFiles(t *testing.T) {
	base := t.TempDir()
	old := userCacheDir
	defer func() { userCacheDir = old }()
	userCacheDir = func() (string, error) { return base, nil }
	dir, err := DataDir("go-numerics/special", strings.Repeat("a", 40))
	if err != nil || !strings.HasPrefix(dir, filepath.Join(base, "refbench", "go-numerics", "special")) {
		t.Fatalf("dir = %q, %v", dir, err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Error("not created")
	}
	for _, c := range [][2]string{{"", "a"}, {"a", ""}, {"..", "a"}, {"a/../b", "c"}, {"a", "b c"}, {"a\\b", "c"}, {".", "c"}, {"é", "c"}} {
		if _, err := DataDir(c[0], c[1]); err == nil {
			t.Errorf("DataDir(%q, %q) accepted", c[0], c[1])
		}
	}
	userCacheDir = func() (string, error) { return filepath.Join(base, ".git"), nil }
	if _, err := DataDir("a", "b"); err == nil {
		t.Error("a .git directory accepted")
	}
	userCacheDir = func() (string, error) { return base + "/sp ace", nil }
	if _, err := DataDir("a", "b"); err == nil {
		t.Error("a space accepted")
	}
	userCacheDir = func() (string, error) { return "", errors.New("no home") }
	if _, err := DataDir("a", "b"); err == nil {
		t.Error("cache dir error lost")
	}

	// WriteF64 / ReadF64 round trip, little-endian column-major.
	p, rows, sha, err := WriteF64(dir, "args", []float64{1, 2}, []float64{3, 4})
	if err != nil || rows != 2 || len(sha) != 64 {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if len(b) != 32 || b[7] != 0x3f || b[15] != 0x40 { // 1.0 and 2.0, little-endian
		t.Errorf("bytes = %x", b)
	}
	got, err := ReadF64(p, 4)
	if err != nil || got[0] != 1 || got[3] != 4 {
		t.Errorf("read = %v %v", got, err)
	}
	if _, err := ReadF64(p, 3); err == nil {
		t.Error("wrong size accepted")
	}
	if _, err := ReadF64(p+".missing", 1); err == nil {
		t.Error("missing file accepted")
	}
	if _, err := ReadF64(p, -1); err == nil {
		t.Error("negative n accepted")
	}
	oldLimit := dataLimit
	dataLimit = 16
	defer func() { dataLimit = oldLimit }()
	if _, err := ReadF64(p, 3); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("an n over the limit accepted: %v", err)
	}
	if _, _, _, err := WriteF64(dir, "big", []float64{1, 2, 3}); err == nil {
		t.Error("a file over the limit accepted")
	}
	dataLimit = oldLimit
	for name, f := range map[string]func() error{
		"bad name": func() error { _, _, _, e := WriteF64(dir, "a/b", []float64{1}); return e },
		"no data":  func() error { _, _, _, e := WriteF64(dir, "a"); return e },
		"empty":    func() error { _, _, _, e := WriteF64(dir, "a", nil); return e },
		"ragged":   func() error { _, _, _, e := WriteF64(dir, "a", []float64{1}, []float64{1, 2}); return e },
		"no dir":   func() error { _, _, _, e := WriteF64(dir+"/missing", "a", []float64{1}); return e },
	} {
		if f() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestBuildWorkerRecordsSourcesAndFlags(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.cpp")
	os.WriteFile(src, []byte("int main(){}\n"), 0o644)
	t.Setenv("REFBENCH_HELPER", "1")
	old := execCommand
	defer func() { execCommand = old }()
	execCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return helperCommand(ctx, "cc", "ok") }
	rec, err := BuildWorker(context.Background(), "c++", filepath.Join(dir, "w"), ReleaseFlags, []string{src, "-lgfortran"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec["src/a.cpp"], "sha256:") || rec["flags"] != "-O2 -lgfortran" {
		t.Errorf("record = %v", rec)
	}
	if _, err := BuildWorker(context.Background(), "c++", "w", nil, nil); err == nil {
		t.Error("no sources accepted")
	}
	if _, err := BuildWorker(context.Background(), "c++", "w", nil, []string{filepath.Join(dir, "missing.cpp")}); err == nil {
		t.Error("missing source accepted")
	}
	execCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return helperCommand(ctx, "cc", "fail") }
	if _, err := BuildWorker(context.Background(), "c++", filepath.Join(dir, "w"), nil, []string{src}); err == nil {
		t.Error("compiler failure lost")
	}
}

// TestCompiledWorkers builds the real harness with the system C++ compiler (and
// the Fortran example with gfortran) and speaks rbw1 with it. Skipped without a
// compiler: the CI of a repository never needs one.
func TestCompiledWorkers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the harness is built and run on POSIX hosts")
	}
	cxx, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("no c++ compiler")
	}
	work := t.TempDir()
	harness := filepath.Join(work, "rbw1.cpp")
	os.WriteFile(harness, []byte(CompiledHarnessSource), 0o644)
	bin := filepath.Join(work, "worker-cpp")
	rec, err := BuildWorker(context.Background(), cxx, bin, []string{"-O2", "-std=c++17"}, []string{harness, "workers/compiled/examples/cpp/kernels.cpp"})
	if err != nil {
		t.Fatal(err)
	}
	if rec["src/kernels.cpp"] == "" {
		t.Error("no digest recorded")
	}
	exerciseCompiled(t, bin, "sqrt")

	gf, err := exec.LookPath("gfortran")
	if err != nil {
		t.Log("no gfortran: Fortran example not built")
		return
	}
	obj := filepath.Join(work, "sqrt.o")
	if out, err := exec.Command(gf, "-O2", "-c", "-o", obj, "workers/compiled/examples/fortran/sqrt.f90").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	binF := filepath.Join(work, "worker-f90")
	if _, err := BuildWorker(context.Background(), cxx, binF, []string{"-O2", "-std=c++17"}, []string{harness, "workers/compiled/examples/fortran/table.cpp", obj, "-lgfortran"}); err != nil {
		t.Skipf("cannot link the Fortran example here: %v", err)
	}
	exerciseCompiled(t, binF, "sqrt")
}

func exerciseCompiled(t *testing.T, bin, fn string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := StartRBW1(context.Background(), RBW1Config{Dir: dir, Funcs: []string{fn}}, nil, bin)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	v, err := w.Versions()
	if err != nil || CheckSingleThread(v) != nil || v["compiler"] == "" {
		t.Fatalf("versions = %v %v", v, err)
	}
	x := []float64{1, 4, 9, -1, 16}
	path, rows, sha, err := WriteF64(dir, "x", x)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Load("x", path, 1, rows, sha); err != nil {
		t.Fatal(err)
	}
	// Negative control: a planted wrong digest is refused with both digests.
	if err := w.Load("x", path, 1, rows, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), sha) {
		t.Errorf("wrong digest: %v", err)
	}
	var nf int
	got, err := w.Evaler("x", fn, filepath.Join(dir, "o.f64"), func(n, _ int) { nf = n })()
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []float64{1, 2, 3, math.NaN(), 4} {
		if got[i] != want && !(math.IsNaN(got[i]) && math.IsNaN(want)) {
			t.Errorf("sqrt(%v) = %v, want %v", x[i], got[i], want)
		}
	}
	if nf != 1 {
		t.Errorf("non-finite = %d, want 1", nf)
	}
	d, err := w.Timer("x", fn)(1000)
	if err != nil || d <= 0 {
		t.Errorf("time = %v %v", d, err)
	}
	if _, err := Calibrate(w.Timer("x", fn), time.Millisecond); err != nil {
		t.Errorf("calibrate: %v", err)
	}
	// The worker refuses what the Go side would not send: a path outside the
	// run directory, a function outside its table, a request that is not ours.
	for _, line := range []string{"L x /etc/passwd 1 1", "T x nofn 1", "W nokey " + fn, "E x " + fn + " /tmp/x", "zz", "L x " + path + " 1", "T x " + fn + " 0", "T x " + fn + " 1  1"} {
		if _, err := w.ask(strings.Split(line, " ")...); err == nil {
			t.Errorf("worker accepted %q", line)
		}
	}
	if _, err := w.ask(strings.Repeat("x", 4100)); err == nil {
		t.Error("oversized request not refused by the client")
	}
	// The harness itself refuses a line over 4096 bytes (bypassing the client).
	io.WriteString(w.in, strings.Repeat("x", 5000)+"\n")
	if !w.out.Scan() || !strings.HasPrefix(w.out.Text(), "err ") {
		t.Errorf("harness reply to an oversized line: %q", w.out.Text())
	}
}

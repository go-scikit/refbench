package refbench

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The rbw1 protocol: one request line, one reply line, plain ASCII, so that R
// base and Fortran (which have no JSON parser) can speak it without a pinned
// dependency. Python keeps its JSON protocol.
//
//	L <key> <path> <cols> <rows>   load float64 little-endian column-major   -> ok <sha256 of the bytes read>
//	W <key> <fn>                   one untimed call (JIT warm-up, page-in)   -> ok
//	T <key> <fn> <iters>           time iters calls inside the worker        -> ns <float>
//	E <key> <fn> <outpath>         evaluate once, write rows float64         -> ok <n_nonfinite> <n_raised>
//	V                              versions and observed threading           -> v k=v k=v ...
//	any line                       on failure                                -> err <message>
//
// Nothing the worker reads from the pipe is evaluated: <fn> is a name in a
// fixed table compiled or defined in the worker, <path> must lie inside the
// run directory, and every size is bounded. Tokens are separated by one space;
// version values replace their spaces by underscores.
const (
	// RBW1MaxLine bounds a line, request or reply, without its newline.
	RBW1MaxLine = 4096
	// RBW1MaxData bounds the bytes of one loaded argument file (256 MiB).
	RBW1MaxData = 256 << 20
	// rbw1MaxDim bounds cols and rows of a request.
	rbw1MaxDim = 1 << 28
	// rbw1MaxVersions bounds the pairs of a version reply.
	rbw1MaxVersions = 256
)

// ErrRBW1LineTooLong is returned when a worker's line is longer than RBW1MaxLine.
var ErrRBW1LineTooLong = errors.New("rbw1: worker line longer than 4096 bytes")

var (
	rbw1KeyPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	rbw1FnPattern  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.]{0,63}$`)
	rbw1SHAPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	rbw1VerKey     = regexp.MustCompile(`^[A-Za-z0-9_./+-]{1,64}$`)
)

// RBW1Reply is one parsed reply line.
type RBW1Reply struct {
	// Kind is "ok", "ns" or "v".
	Kind string
	// Args are the tokens after "ok".
	Args []string
	// NS is the total nanoseconds of a "ns" reply.
	NS float64
	// Versions are the pairs of a "v" reply.
	Versions map[string]string
}

func printableASCII(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// ParseRBW1Reply reads one worker line strictly: at most RBW1MaxLine printable
// ASCII bytes, tokens separated by exactly one space, a finite non-negative
// time in a "ns" reply, unique well-formed pairs in a "v" reply. An "err" line
// is an error.
func ParseRBW1Reply(line []byte) (RBW1Reply, error) {
	var r RBW1Reply
	switch {
	case len(line) > RBW1MaxLine:
		return r, ErrRBW1LineTooLong
	case !printableASCII(line):
		return r, errors.New("rbw1: reply is not printable ASCII")
	}
	s := string(line)
	kind, rest, hasRest := strings.Cut(s, " ")
	if kind == "err" {
		return r, fmt.Errorf("reference failed: %s", rest)
	}
	var toks []string
	if hasRest {
		toks = strings.Split(rest, " ")
		for _, t := range toks {
			if t == "" {
				return r, errors.New("rbw1: empty token (a doubled, leading or trailing space)")
			}
		}
	}
	r.Kind = kind
	switch kind {
	case "ok":
		r.Args = toks
	case "ns":
		if len(toks) != 1 {
			return RBW1Reply{}, errors.New("rbw1: ns reply needs one number")
		}
		f, err := strconv.ParseFloat(toks[0], 64)
		if err != nil || !finiteNonNeg(f) {
			return RBW1Reply{}, errors.New("rbw1: ns is not a finite non-negative number")
		}
		r.NS = f
	case "v":
		if len(toks) == 0 || len(toks) > rbw1MaxVersions {
			return RBW1Reply{}, errors.New("rbw1: v reply needs 1 to 256 pairs")
		}
		r.Versions = make(map[string]string, len(toks))
		for _, t := range toks {
			k, v, ok := strings.Cut(t, "=")
			if !ok || !rbw1VerKey.MatchString(k) || v == "" {
				return RBW1Reply{}, fmt.Errorf("rbw1: bad version pair %q", t)
			}
			if _, dup := r.Versions[k]; dup {
				return RBW1Reply{}, fmt.Errorf("rbw1: version key %q twice", k)
			}
			r.Versions[k] = v
		}
	default:
		return RBW1Reply{}, fmt.Errorf("rbw1: unknown reply %q", kind)
	}
	return r, nil
}

// CheckSingleThread refuses a worker whose reply to V does not show one thread:
// the key "threads" must be present, and every key ending in "threads"
// (gc_threads, blas_threads, ...) must be 1. The environment variables that ask
// for one thread are not trusted, what the worker observed is.
func CheckSingleThread(v map[string]string) error {
	if v["threads"] == "" {
		return errors.New("rbw1: the worker does not report its thread count (key threads)")
	}
	for k, val := range v {
		if (k == "threads" || strings.HasSuffix(k, "_threads")) && val != "1" {
			return fmt.Errorf("rbw1: the worker runs with %s=%s, not 1: the timing would not be single-core", k, val)
		}
	}
	return nil
}

// NamespaceVersions prefixes the keys of a worker's versions with its kind
// ("julia/Bessels"), the form ref_versions records.
func NamespaceVersions(kind string, v map[string]string) map[string]string {
	out := make(map[string]string, len(v))
	for k, val := range v {
		out[kind+"/"+k] = val
	}
	return out
}

// RBW1Config describes the run directory and the function table of a worker.
type RBW1Config struct {
	// Dir is the absolute run directory: every path of a request lies inside it.
	Dir string
	// Funcs is the worker's function table. A request for another name is
	// refused before it is sent.
	Funcs []string
}

// RBW1 is a long-lived worker speaking rbw1.
type RBW1 struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Scanner
	dir    string
	funcs  map[string]bool
	rows   map[string]int
	warmed map[string]bool
}

// NewRBW1 speaks rbw1 over a pair of streams (tests, or a process started by
// StartRBW1).
func NewRBW1(in io.WriteCloser, out io.Reader, cfg RBW1Config) (*RBW1, error) {
	if !filepath.IsAbs(cfg.Dir) || filepath.Clean(cfg.Dir) != cfg.Dir || !printableASCII([]byte(cfg.Dir)) || strings.Contains(cfg.Dir, " ") {
		return nil, fmt.Errorf("rbw1: run directory %q is not a clean absolute path without spaces", cfg.Dir)
	}
	if len(cfg.Funcs) == 0 {
		return nil, errors.New("rbw1: no function table")
	}
	w := &RBW1{in: in, dir: cfg.Dir, funcs: map[string]bool{}, rows: map[string]int{}, warmed: map[string]bool{}}
	for _, f := range cfg.Funcs {
		if !rbw1FnPattern.MatchString(f) {
			return nil, fmt.Errorf("rbw1: function name %q is not a plain identifier", f)
		}
		w.funcs[f] = true
	}
	sc := bufio.NewScanner(out)
	// One byte more than a line and its newline: a longer line is refused
	// without ever being buffered.
	sc.Buffer(make([]byte, 0, RBW1MaxLine+2), RBW1MaxLine+2)
	w.out = sc
	return w, nil
}

// StartRBW1 starts a worker process under env (nil: SingleThreadEnv) and speaks
// rbw1 with it. The run directory is passed to the worker as RBW1_DIR. The
// worker's stderr goes to the caller's.
func StartRBW1(ctx context.Context, cfg RBW1Config, env []string, name string, args ...string) (*RBW1, error) {
	cmd := execCommand(ctx, name, args...)
	if env == nil {
		env = SingleThreadEnv()
	}
	cmd.Env = append(env, "RBW1_DIR="+cfg.Dir)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	w, err := NewRBW1(in, out, cfg)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w.cmd = cmd
	return w, nil
}

func (w *RBW1) ask(parts ...string) (RBW1Reply, error) {
	line := strings.Join(parts, " ")
	if len(line) > RBW1MaxLine {
		return RBW1Reply{}, errors.New("rbw1: request longer than 4096 bytes")
	}
	if _, err := io.WriteString(w.in, line+"\n"); err != nil {
		return RBW1Reply{}, fmt.Errorf("worker: %w", err)
	}
	if !w.out.Scan() {
		if err := w.out.Err(); err != nil {
			if errors.Is(err, bufio.ErrTooLong) {
				return RBW1Reply{}, ErrRBW1LineTooLong
			}
			return RBW1Reply{}, fmt.Errorf("worker: %w", err)
		}
		return RBW1Reply{}, errors.New("worker: process ended")
	}
	return ParseRBW1Reply(w.out.Bytes())
}

func (w *RBW1) check(key, fn string) error {
	if !rbw1KeyPattern.MatchString(key) {
		return fmt.Errorf("rbw1: key %q is not a plain identifier", key)
	}
	if !w.funcs[fn] {
		return fmt.Errorf("rbw1: function %q is not in the worker's table", fn)
	}
	// Only a key whose LAST Load verified the digest may be warmed, timed or
	// evaluated: a mismatch, or a reload that failed after an earlier success,
	// leaves the worker holding data that is not the Go side's.
	if _, ok := w.rows[key]; !ok {
		return fmt.Errorf("rbw1: key %q has no verified load", key)
	}
	return nil
}

// inDir reports whether path is a clean absolute path strictly inside the run
// directory, without spaces.
func (w *RBW1) inDir(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !printableASCII([]byte(path)) ||
		strings.Contains(path, " ") || !strings.HasPrefix(path, w.dir+string(filepath.Separator)) {
		return fmt.Errorf("rbw1: path %q is not a clean absolute path inside %s", path, w.dir)
	}
	return nil
}

// Load asks the worker to read an argument file (cols columns of rows float64,
// column-major, little-endian) and compares the SHA-256 of the bytes it read
// with want. A mismatch is an error naming both digests: nothing is timed on
// data that did not arrive unaltered.
func (w *RBW1) Load(key, path string, cols, rows int, want string) error {
	if !rbw1KeyPattern.MatchString(key) {
		return fmt.Errorf("rbw1: key %q is not a plain identifier", key)
	}
	delete(w.rows, key) // verified again only if this load succeeds
	if err := w.inDir(path); err != nil {
		return err
	}
	if cols <= 0 || rows <= 0 || cols > rbw1MaxDim || rows > rbw1MaxDim || int64(cols)*int64(rows)*8 > RBW1MaxData {
		return fmt.Errorf("rbw1: %d x %d float64 is outside 1..%d bytes", cols, rows, RBW1MaxData)
	}
	r, err := w.ask("L", key, path, strconv.Itoa(cols), strconv.Itoa(rows))
	if err != nil {
		return err
	}
	if r.Kind != "ok" || len(r.Args) != 1 || !rbw1SHAPattern.MatchString(r.Args[0]) {
		return errors.New("rbw1: load reply is not 'ok <sha256>'")
	}
	if r.Args[0] != want {
		return fmt.Errorf("rbw1: the worker read %s, the Go side wrote %s: the inputs did not arrive unaltered", r.Args[0], want)
	}
	w.rows[key] = rows
	return nil
}

func (w *RBW1) okReply(parts ...string) (RBW1Reply, error) {
	r, err := w.ask(parts...)
	if err == nil && r.Kind != "ok" {
		err = fmt.Errorf("rbw1: reply %q to %s, want ok", r.Kind, parts[0])
	}
	return r, err
}

// Warm asks for one untimed call of fn on the loaded key (JIT compilation,
// page-in).
func (w *RBW1) Warm(key, fn string) error {
	if err := w.check(key, fn); err != nil {
		return err
	}
	r, err := w.okReply("W", key, fn)
	if err == nil && len(r.Args) != 0 {
		err = errors.New("rbw1: warm-up reply has arguments")
	}
	return err
}

// Time asks for iters calls of fn on the loaded key, timed inside the worker.
func (w *RBW1) Time(key, fn string, iters int) (time.Duration, error) {
	if err := w.check(key, fn); err != nil {
		return 0, err
	}
	if iters <= 0 {
		return 0, errors.New("rbw1: iters must be positive")
	}
	r, err := w.ask("T", key, fn, strconv.Itoa(iters))
	if err != nil {
		return 0, err
	}
	if r.Kind != "ns" {
		return 0, fmt.Errorf("rbw1: reply %q to T, want ns", r.Kind)
	}
	if r.NS >= math.MaxInt64 {
		return 0, errors.New("rbw1: ns overflows")
	}
	return time.Duration(r.NS), nil
}

// Eval asks for one evaluation of fn on every point of the loaded key, written
// as rows float64 to outPath. It returns the number of non-finite values and of
// points the reference refused (domain errors, which the worker turns into NaN).
func (w *RBW1) Eval(key, fn, outPath string) (nonFinite, raised int, err error) {
	if err := w.check(key, fn); err != nil {
		return 0, 0, err
	}
	if err := w.inDir(outPath); err != nil {
		return 0, 0, err
	}
	r, err := w.okReply("E", key, fn, outPath)
	if err != nil {
		return 0, 0, err
	}
	if len(r.Args) != 2 {
		return 0, 0, errors.New("rbw1: eval reply is not 'ok <n_nonfinite> <n_raised>'")
	}
	a, err1 := strconv.Atoi(r.Args[0])
	b, err2 := strconv.Atoi(r.Args[1])
	if err1 != nil || err2 != nil || a < 0 || b < 0 {
		return 0, 0, errors.New("rbw1: eval counts are not non-negative integers")
	}
	return a, b, nil
}

// Versions asks for the versions and the observed threading.
func (w *RBW1) Versions() (map[string]string, error) {
	r, err := w.ask("V")
	if err != nil {
		return nil, err
	}
	if r.Kind != "v" {
		return nil, fmt.Errorf("rbw1: reply %q to V, want v", r.Kind)
	}
	return r.Versions, nil
}

// Timer returns a Timer of fn on the loaded key. The first call is preceded by
// one untimed W, so that the interval of the first timed call is not the
// compilation of the kernel.
func (w *RBW1) Timer(key, fn string) Timer {
	return func(iters int) (time.Duration, error) {
		if id := key + "/" + fn; !w.warmed[id] {
			if err := w.Warm(key, fn); err != nil {
				return 0, err
			}
			w.warmed[id] = true
		}
		return w.Time(key, fn, iters)
	}
}

// Evaler returns an EvalFunc of fn on the loaded key: the worker writes its
// values to outPath, which is read back (and must hold exactly one value per
// row). The counts of non-finite values and refused points are reported to
// count when it is not nil.
func (w *RBW1) Evaler(key, fn, outPath string, count func(nonFinite, raised int)) EvalFunc {
	return func() ([]float64, error) {
		rows, ok := w.rows[key]
		if !ok {
			return nil, fmt.Errorf("rbw1: key %q was not loaded", key)
		}
		nf, rs, err := w.Eval(key, fn, outPath)
		if err != nil {
			return nil, err
		}
		if count != nil {
			count(nf, rs)
		}
		return ReadF64(outPath, rows)
	}
}

// Close closes the worker's input and waits for the process, if there is one.
func (w *RBW1) Close() error {
	err := w.in.Close()
	if w.cmd != nil {
		return w.cmd.Wait()
	}
	return err
}

// Argument files.

// userCacheDir is replaceable by tests.
var userCacheDir = os.UserCacheDir

// DataDir returns (and creates) the run directory of a repository's commit:
// os.UserCacheDir()/refbench/<repo>/<commit>. It belongs to the tool: not to
// the repository, not to /tmp. repo may contain one "/" ("go-x/y").
func DataDir(repo, commit string) (string, error) {
	for _, s := range []string{repo, commit} {
		if s == "" || s == "." || strings.Contains(s, "..") || strings.ContainsAny(s, " \\\x00") || !printableASCII([]byte(s)) {
			return "", fmt.Errorf("data dir: %q is not a plain name", s)
		}
	}
	base, err := userCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "refbench", filepath.FromSlash(repo), commit)
	for _, e := range strings.Split(filepath.ToSlash(dir), "/") {
		if e == ".git" {
			return "", fmt.Errorf("data dir %s is inside a .git directory", dir)
		}
	}
	if strings.Contains(dir, " ") {
		return "", fmt.Errorf("data dir %s contains a space (rbw1 tokens are space separated)", dir)
	}
	return dir, os.MkdirAll(dir, 0o700)
}

// dataLimit is RBW1MaxData; tests lower it to reach the refusal without
// allocating 256 MiB.
var dataLimit int64 = RBW1MaxData

// WriteF64 writes cols (equal lengths) as float64 little-endian column-major to
// dir/name and returns the path, the number of rows and the SHA-256 of the
// bytes: what Load compares with the worker's. A file over RBW1MaxData is
// refused.
func WriteF64(dir, name string, cols ...[]float64) (path string, rows int, sha string, err error) {
	if !rbw1KeyPattern.MatchString(name) {
		return "", 0, "", fmt.Errorf("argument file name %q is not a plain identifier", name)
	}
	if len(cols) == 0 || len(cols[0]) == 0 {
		return "", 0, "", errors.New("no data to write")
	}
	rows = len(cols[0])
	if int64(len(cols))*int64(rows)*8 > dataLimit {
		return "", 0, "", fmt.Errorf("argument file larger than %d bytes", dataLimit)
	}
	buf := make([]byte, 0, len(cols)*rows*8)
	for _, c := range cols {
		if len(c) != rows {
			return "", 0, "", errors.New("columns of different lengths")
		}
		for _, x := range c {
			buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(x))
		}
	}
	path = filepath.Join(dir, name+".f64")
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return "", 0, "", err
	}
	sum := sha256.Sum256(buf)
	return path, rows, hex.EncodeToString(sum[:]), nil
}

// ReadF64 reads exactly n float64 little-endian values from path.
func ReadF64(path string, n int) ([]float64, error) {
	if n < 0 || int64(n)*8 > dataLimit {
		return nil, errors.New("rbw1: result size out of range")
	}
	// The size is checked before the read: a worker cannot make us buffer more.
	var b []byte
	st, err := os.Stat(path)
	if err == nil && st.Size() != int64(n)*8 {
		err = fmt.Errorf("rbw1: %s holds %d bytes, want %d", path, st.Size(), int64(n)*8)
	}
	if err == nil {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = math.Float64frombits(binary.LittleEndian.Uint64(b[8*i:]))
	}
	return out, nil
}

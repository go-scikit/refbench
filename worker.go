package refbench

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"time"
)

// PyWorkerSource is the Python program of the reference worker: one JSON
// request per line, one JSON reply per line (see the file for the protocol).
//
//go:embed pyworker.py
var PyWorkerSource string

// PythonVersion is the interpreter the references run on (design, section 3.2).
const PythonVersion = "3.12.12"

// SingleThreadEnv returns the environment of a reference process: the current
// one with every threaded numerical library pinned to one thread, the set that
// go-simd/floats' bench/numpycmp already uses.
func SingleThreadEnv() []string {
	return append(os.Environ(),
		"OMP_NUM_THREADS=1", "OPENBLAS_NUM_THREADS=1", "MKL_NUM_THREADS=1", "VECLIB_MAXIMUM_THREADS=1")
}

// UVArgs returns the arguments of the uv command that runs the Python worker
// with the pinned, hash-checked requirements file (refs.txt):
//
//	uv run --no-project --python 3.12.12 --with-requirements refs.txt python -I -c <worker>
func UVArgs(requirements string) []string {
	return []string{"run", "--no-project", "--python", PythonVersion,
		"--with-requirements", requirements, "python", "-I", "-c", PyWorkerSource}
}

// Request is one line sent to a worker. A C worker reads the same fields: Call
// names its kernel, Setup its input formula.
type Request struct {
	Setup    string `json:"setup,omitempty"`
	Call     string `json:"call,omitempty"`
	N        int    `json:"n,omitempty"`
	Iters    int    `json:"iters,omitempty"`
	Hash     bool   `json:"hash,omitempty"`
	Versions bool   `json:"versions,omitempty"`
}

// Reply is one line a worker answers.
type Reply struct {
	// NS is the total nanoseconds of Iters calls, timed in the worker.
	NS       *float64          `json:"ns,omitempty"`
	Hash     string            `json:"hash,omitempty"`
	Versions map[string]string `json:"versions,omitempty"`
	Error    string            `json:"error,omitempty"`
}

// maxReplyBytes bounds a worker's line.
const maxReplyBytes = 1 << 20

// ParseReply reads one worker line strictly: a JSON object with known fields,
// no trailing data, a finite non-negative time, and no error field.
func ParseReply(line []byte) (Reply, error) {
	var r Reply
	if len(line) > maxReplyBytes {
		return r, errors.New("worker reply too long")
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Reply{}, fmt.Errorf("worker reply: %w", err)
	}
	if dec.More() {
		return Reply{}, errors.New("worker reply: trailing data")
	}
	if r.Error != "" {
		return Reply{}, fmt.Errorf("reference failed: %s", r.Error)
	}
	if r.NS != nil && !finiteNonNeg(*r.NS) {
		return Reply{}, errors.New("worker reply: ns is not a finite non-negative number")
	}
	return r, nil
}

// Duration returns the reply's timed interval.
func (r Reply) Duration() (time.Duration, error) {
	if r.NS == nil {
		return 0, errors.New("worker reply has no ns")
	}
	if *r.NS >= math.MaxInt64 {
		return 0, errors.New("worker reply: ns overflows")
	}
	return time.Duration(*r.NS), nil
}

// Worker is a long-lived reference process speaking JSON lines.
type Worker struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Scanner
}

// execCommand is replaceable by tests.
var execCommand = exec.CommandContext

// StartWorker starts name with args under SingleThreadEnv. The worker's stderr
// goes to the caller's.
func StartWorker(ctx context.Context, name string, args ...string) (*Worker, error) {
	cmd := execCommand(ctx, name, args...)
	cmd.Env = SingleThreadEnv()
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64<<10), maxReplyBytes)
	return &Worker{cmd: cmd, in: in, out: sc}, nil
}

// StartPython starts the Python worker through uv with the requirements file.
func StartPython(ctx context.Context, uv, requirements string) (*Worker, error) {
	return StartWorker(ctx, uv, UVArgs(requirements)...)
}

// Ask sends one request and reads its reply.
func (w *Worker) Ask(req Request) (Reply, error) {
	b, _ := json.Marshal(req) // a struct of strings, ints and bools cannot fail
	if _, err := w.in.Write(append(b, '\n')); err != nil {
		return Reply{}, fmt.Errorf("worker: %w", err)
	}
	if !w.out.Scan() {
		if err := w.out.Err(); err != nil {
			return Reply{}, fmt.Errorf("worker: %w", err)
		}
		return Reply{}, errors.New("worker: process ended")
	}
	return ParseReply(w.out.Bytes())
}

// Timer returns a Timer for one case: it asks the worker to time iters calls of
// the reference expression on the arguments built by setup for size n.
func (w *Worker) Timer(setup, call string, n int) Timer {
	return func(iters int) (time.Duration, error) {
		r, err := w.Ask(Request{Setup: setup, Call: call, N: n, Iters: iters})
		if err != nil {
			return 0, err
		}
		return r.Duration()
	}
}

// DataHash asks for the SHA-256 of the arguments the reference receives, to
// compare with the hash of the Go side's data.
func (w *Worker) DataHash(setup string, n int) (string, error) {
	r, err := w.Ask(Request{Setup: setup, N: n, Hash: true})
	if err != nil {
		return "", err
	}
	if r.Hash == "" {
		return "", errors.New("worker reply has no hash")
	}
	return r.Hash, nil
}

// RefVersions asks for the reference versions (numpy, scipy, blas, ...) that go
// in the table header.
func (w *Worker) RefVersions() (map[string]string, error) {
	r, err := w.Ask(Request{Versions: true})
	if err != nil {
		return nil, err
	}
	if len(r.Versions) == 0 {
		return nil, errors.New("worker reply has no versions")
	}
	return r.Versions, nil
}

// Close closes the worker's input and waits for it to exit.
func (w *Worker) Close() error {
	w.in.Close()
	return w.cmd.Wait()
}

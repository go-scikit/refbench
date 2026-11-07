package refbench

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// helperCommand re-executes the test binary as a fake worker or compiler.
func helperCommand(ctx context.Context, mode string, extra ...string) *exec.Cmd {
	args := append([]string{"-test.run=TestHelperProcess", "--", mode}, extra...)
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	return cmd
}

// TestHelperProcess is not a test: it is the fake worker the others start.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("REFBENCH_HELPER") == "" {
		return
	}
	mode := ""
	for i, a := range os.Args {
		if a == "--" {
			mode = os.Args[i+1]
			if mode == "cc" && os.Args[i+2] == "fail" {
				fmt.Println("compiler said no")
				os.Exit(1)
			}
		}
	}
	if mode == "cc" {
		os.Exit(0)
	}
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var r Request
		_ = json.Unmarshal(in.Bytes(), &r)
		switch {
		case mode == "die":
			os.Exit(0)
		case mode == "empty":
			fmt.Println(`{}`)
		case mode == "long":
			fmt.Println(strings.Repeat("x", 2<<20))
		case r.Versions:
			fmt.Println(`{"versions":{"numpy":"2.5.4"}}`)
		case r.Hash && r.Setup == "nohash":
			fmt.Println(`{}`)
		case r.Hash:
			fmt.Println(`{"hash":"abc"}`)
		case r.Call == "boom":
			fmt.Println(`{"error":"ValueError: boom"}`)
		case r.Call == "nons":
			fmt.Println(`{}`)
		case r.Setup == "novers":
			fmt.Println(`{}`)
		default:
			fmt.Printf("{\"ns\":%d}\n", r.Iters*1000)
		}
	}
	os.Exit(0)
}

func startHelper(t *testing.T, mode string) *Worker {
	t.Helper()
	t.Setenv("REFBENCH_HELPER", "1")
	old := execCommand
	t.Cleanup(func() { execCommand = old })
	execCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return helperCommand(ctx, mode) }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // kills a helper blocked on a full pipe
	w, err := StartWorker(ctx, "ignored")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWorkerRoundTrip(t *testing.T) {
	w := startHelper(t, "echo")
	d, err := w.Timer("()", "f", 10)(7)
	if err != nil || d.Nanoseconds() != 7000 {
		t.Errorf("timer = %v, %v", d, err)
	}
	if h, err := w.DataHash("()", 10); err != nil || h != "abc" {
		t.Errorf("hash = %q, %v", h, err)
	}
	if _, err := w.DataHash("nohash", 10); err == nil {
		t.Error("empty hash accepted")
	}
	if v, err := w.RefVersions(); err != nil || v["numpy"] != "2.5.4" {
		t.Errorf("versions = %v, %v", v, err)
	}
	if _, err := w.Timer("()", "boom", 1)(1); err == nil || !strings.Contains(err.Error(), "ValueError: boom") {
		t.Errorf("reference error = %v", err)
	}
	if _, err := w.Timer("()", "nons", 1)(1); err == nil {
		t.Error("reply without ns accepted")
	}
	if _, err := w.Ask(Request{Setup: "novers", Versions: false, Call: "x"}); err != nil {
		t.Error(err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

func TestWorkerNoVersions(t *testing.T) {
	w := startHelper(t, "echo")
	defer w.Close()
	// "versions" with an empty map: ask through a request the helper answers with {}.
	r, err := w.Ask(Request{Setup: "novers"})
	if err != nil || len(r.Versions) != 0 {
		t.Fatalf("%v %v", r, err)
	}
}

func TestWorkerFailures(t *testing.T) {
	w := startHelper(t, "die")
	if _, err := w.Ask(Request{Call: "x"}); err == nil || !strings.Contains(err.Error(), "ended") {
		t.Errorf("ended: %v", err)
	}
	w.Close()

	w = startHelper(t, "die")
	if _, err := w.DataHash("()", 1); err == nil {
		t.Error("dead worker gave a hash")
	}
	w.Close()
	w = startHelper(t, "die")
	if _, err := w.RefVersions(); err == nil {
		t.Error("dead worker gave versions")
	}
	w.Close()
	w = startHelper(t, "empty")
	if _, err := w.RefVersions(); err == nil {
		t.Error("empty versions accepted")
	}
	w.Close()

	w = startHelper(t, "long")
	if _, err := w.Ask(Request{Call: "x"}); err == nil {
		t.Error("too-long line accepted")
	}
	// The helper is blocked on a full pipe: the cleanup's cancel kills it.

	w = startHelper(t, "echo")
	w.in.Close()
	if _, err := w.Ask(Request{Call: "x"}); err == nil {
		t.Error("write to a closed worker accepted")
	}
	w.cmd.Wait()
}

func TestStartWorkerErrors(t *testing.T) {
	if _, err := StartWorker(context.Background(), "/nonexistent/refbench-worker"); err == nil {
		t.Error("missing binary accepted")
	}
	if _, err := StartPython(context.Background(), "/nonexistent/uv", "refs.txt"); err == nil {
		t.Error("missing uv accepted")
	}
	old := execCommand
	defer func() { execCommand = old }()
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, name, args...)
		c.Stdin = strings.NewReader("")
		return c
	}
	if _, err := StartWorker(context.Background(), "x"); err == nil {
		t.Error("stdin pipe error not reported")
	}
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, name, args...)
		c.Stdout = os.Stderr
		return c
	}
	if _, err := StartWorker(context.Background(), "x"); err == nil {
		t.Error("stdout pipe error not reported")
	}
}

func TestParseReply(t *testing.T) {
	good := map[string]float64{`{"ns":12.5}`: 12.5, `{"ns":0}`: 0}
	for in, ns := range good {
		r, err := ParseReply([]byte(in))
		if err != nil || r.NS == nil || *r.NS != ns {
			t.Errorf("%s: %v %v", in, r, err)
		}
		if _, err := r.Duration(); err != nil {
			t.Error(err)
		}
	}
	for _, in := range []string{``, `[]`, `{"ns":-1}`, `{"ns":1e999}`, `{"nope":1}`, `{"ns":1} {"ns":2}`, `{"error":"x"}`, `{"ns":"1"}`} {
		if _, err := ParseReply([]byte(in)); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	if _, err := ParseReply(make([]byte, maxReplyBytes+1)); err == nil {
		t.Error("oversized accepted")
	}
	if _, err := (Reply{}).Duration(); err == nil {
		t.Error("no ns accepted")
	}
	big := 1e300
	if _, err := (Reply{NS: &big}).Duration(); err == nil {
		t.Error("overflow accepted")
	}
}

func FuzzParseReply(f *testing.F) {
	f.Add([]byte(`{"ns":12.5}`))
	f.Add([]byte(`{"hash":"abc","versions":{"numpy":"2.5.4"}}`))
	f.Add([]byte(`{"error":"x"}`))
	f.Add([]byte(`{"ns":1e999}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseReply(b)
		if err != nil {
			return
		}
		if r.NS != nil && (*r.NS < 0 || *r.NS != *r.NS) {
			t.Fatalf("accepted ns %v", *r.NS)
		}
		_, _ = r.Duration()
	})
}

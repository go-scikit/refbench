package refbench

import (
	"context"
	"os"
	"testing"
)

// TestPythonWorker runs the real worker through uv with the pinned refs.txt. It
// needs the network and uv, so it runs only when REFBENCH_UV names the uv binary.
// It checks the protocol, not a time.
func TestPythonWorker(t *testing.T) {
	uv := os.Getenv("REFBENCH_UV")
	if uv == "" {
		t.Skip("REFBENCH_UV is not set")
	}
	w, err := StartPython(context.Background(), uv, "refs.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	v, err := w.RefVersions()
	if err != nil || v["numpy"] != "2.5.4" || v["scipy"] != "1.18.1" {
		t.Fatalf("versions = %v, %v", v, err)
	}
	t.Logf("versions: %v", v)
	h1, err := w.DataHash("(np.arange(n, dtype=np.float64),)", 100)
	if err != nil {
		t.Fatal(err)
	}
	if h2, _ := w.DataHash("(np.arange(n, dtype=np.float64),)", 100); h1 != h2 || len(h1) != 64 {
		t.Errorf("hash not stable: %s %s", h1, h2)
	}
	d, err := w.Timer("(np.arange(n, dtype=np.float64),)", "lambda x: np.sum(x)", 100)(10)
	if err != nil || d <= 0 {
		t.Errorf("timer = %v, %v", d, err)
	}
	if _, err := w.Timer("()", "lambda: 1/0", 1)(1); err == nil {
		t.Error("a raising reference was not reported")
	}
}

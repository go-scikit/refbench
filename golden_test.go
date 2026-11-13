package refbench

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/golden (only to record a deliberate schema 1 change)")

// TestGoldenSchema1 pins the bytes a repository with no extra reference writes:
// perf.json and perf-table.md must stay byte for byte what they were before
// several references per case existed (the files in testdata/golden were
// written by the code of v0.3.x, before Case.Refs).
func TestGoldenSchema1(t *testing.T) {
	r := sampleReport()
	var js bytes.Buffer
	if err := r.Write(&js); err != nil {
		t.Fatal(err)
	}
	table := Table(r)
	for name, got := range map[string][]byte{"schema1.json": js.Bytes(), "schema1-table.md": table} {
		path := filepath.Join("testdata", "golden", name)
		if *updateGolden {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from its golden file: schema 1 output changed", name)
		}
	}
}

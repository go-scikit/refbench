package rbtest

import (
	"runtime/debug"
	"testing"
)

// The tests replace flagSet and buildModule; these keep the originals, taken
// at package initialisation, so that their bodies run too (covblocks lists
// every block no test entered).
var origFlagSet, origBuildModule = flagSet, buildModule

func TestDefaultFlagSetAndBuildModule(t *testing.T) {
	if origFlagSet("perf.run") {
		t.Error("perf.run reported as given on the command line")
	}
	if got := origBuildModule(); got == "" {
		t.Error("the test binary has no module path")
	}
	old := readBuildInfo
	defer func() { readBuildInfo = old }()
	readBuildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
	if got := origBuildModule(); got != "" {
		t.Errorf("module = %q without build info", got)
	}
}

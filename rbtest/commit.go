package rbtest

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitState is what git says about the tree being measured.
type gitState struct {
	Head  string   // the 40-character SHA of HEAD
	Dirty []string // paths `git status --porcelain` lists
}

// gitLookup reads the commit and the dirty paths of the tree at root; tests
// replace it.
var gitLookup = func(root string) (gitState, error) {
	head, err := gitOut(root, "rev-parse", "HEAD")
	if err != nil {
		return gitState{}, err
	}
	status, err := gitOut(root, "status", "--porcelain")
	if err != nil {
		return gitState{}, err
	}
	st := gitState{Head: strings.TrimSpace(head)}
	for _, line := range strings.Split(status, "\n") {
		if len(line) > 3 {
			st.Dirty = append(st.Dirty, strings.TrimSpace(line[3:]))
		}
	}
	return st, nil
}

func gitOut(root string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// harnessFile reports whether a dirty path is the bench module's go.mod or
// go.sum: consumers bump refbench locally, and those files are the harness,
// not the library.
func harnessFile(p string) bool {
	switch filepath.ToSlash(p) {
	case "bench/go.mod", "bench/go.sum":
		return true
	}
	return false
}

// resolveCommit returns the commit to record. With flag set, it must equal
// the 40-character HEAD of the tree at root and the tree must be clean
// (outside the harness files). Without it, HEAD is filled in, suffixed
// "-dirty" when the tree is; with no git and no flag, the commit stays empty.
func resolveCommit(root, flag string) (string, error) {
	st, err := gitLookup(root)
	if err != nil {
		if flag != "" {
			return "", fmt.Errorf("-perf.commit %q cannot be checked: %w", flag, err)
		}
		return "", nil
	}
	var dirty []string
	for _, p := range st.Dirty {
		if !harnessFile(p) {
			dirty = append(dirty, p)
		}
	}
	if flag != "" {
		if flag != st.Head {
			return "", fmt.Errorf("-perf.commit is %q, but HEAD of %s is %q: give the full 40-character SHA of the tree measured (git rev-parse HEAD), or omit the flag", flag, root, st.Head)
		}
		if len(dirty) > 0 {
			return "", fmt.Errorf("-perf.commit %q given but the tree at %s is dirty, so a table would claim a commit it did not measure; dirty paths: %s", flag, root, strings.Join(dirty, ", "))
		}
		return flag, nil
	}
	if len(dirty) > 0 {
		return st.Head + "-dirty", nil
	}
	return st.Head, nil
}

package adaptertest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// smolvm refuses every guest request whose headers or body carry its
// credential-placeholder prefix. An agent sends what it reads to its model,
// so a tracked file that spells the prefix out ends the session of any
// agent that reads it. The prefix is only ever built from these two halves.
const (
	placeholderPrefixHead = "SMOL_"
	placeholderPrefixTail = "PLACEHOLDER_"
)

type placeholderLiteralReporter interface {
	Errorf(format string, args ...any)
}

// checkNoPlaceholderLiteral reports every file among paths, relative to
// root, that contains the joined placeholder prefix, and returns how many
// files it read.
func checkNoPlaceholderLiteral(r placeholderLiteralReporter, root string, paths []string) int {
	needle := []byte(placeholderPrefixHead + placeholderPrefixTail)
	read := 0
	for _, rel := range paths {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			r.Errorf("read %s: %v", rel, err)
			continue
		}
		read++
		if bytes.Contains(data, needle) {
			line := 1 + bytes.Count(data[:bytes.Index(data, needle)], []byte("\n"))
			r.Errorf("%s:%d spells out smolvm's credential-placeholder prefix; describe it instead, or build it from two halves at run time", rel, line)
		}
	}
	return read
}

// TestNoPlaceholderLiteralInTrackedFiles asserts that no tracked file spells
// out smolvm's credential-placeholder prefix.
func TestNoPlaceholderLiteralInTrackedFiles(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Skip("not a git checkout; tracked files are unknown")
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var paths []string
	for p := range strings.SplitSeq(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	if read := checkNoPlaceholderLiteral(t, root, paths); read == 0 {
		t.Fatal("checkNoPlaceholderLiteral read zero tracked files, want at least one")
	}
}

type placeholderLiteralFakeReporter struct {
	errors []string
}

func (f *placeholderLiteralFakeReporter) Errorf(format string, _ ...any) {
	f.errors = append(f.errors, format)
}

// TestNoPlaceholderLiteral_CatchesRealBreaks proves the guard can fail on a
// scratch file that carries the joined prefix, and passes on one that
// carries only its halves apart.
func TestNoPlaceholderLiteral_CatchesRealBreaks(t *testing.T) {
	dir := t.TempDir()
	bad := "line one\nkey=" + placeholderPrefixHead + placeholderPrefixTail + "abc\n"
	good := "head " + placeholderPrefixHead + " tail " + placeholderPrefixTail + "\n"
	if err := os.WriteFile(filepath.Join(dir, "bad.md"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "good.md"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}

	reporter := &placeholderLiteralFakeReporter{}
	if read := checkNoPlaceholderLiteral(reporter, dir, []string{"bad.md", "good.md"}); read != 2 {
		t.Fatalf("checkNoPlaceholderLiteral read %d files, want 2", read)
	}
	if len(reporter.errors) != 1 {
		t.Fatalf("checkNoPlaceholderLiteral recorded %d failures, want 1 for bad.md only", len(reporter.errors))
	}
}

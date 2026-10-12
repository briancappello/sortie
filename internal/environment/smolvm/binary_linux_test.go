//go:build linux

package smolvm

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// copyOfTestBinary returns a copy of the running test binary, a Linux
// executable for this host, at a path of its own.
func copyOfTestBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe) //nolint:gosec // G304: the test binary
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sortie")
	if err := os.WriteFile(path, data, 0o755); err != nil { //nolint:gosec // G306: an executable
		t.Fatal(err)
	}
	return path
}

func binaryEnv(source string) *Environment {
	return &Environment{sortieBinary: source, logger: slog.New(slog.DiscardHandler)}
}

func TestEnsureBinary_HoldsOnlyACopyOfTheSource(t *testing.T) {
	t.Parallel()
	source := copyOfTestBinary(t)
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(binDir, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := binaryEnv(source).ensureBinary(binDir); err != nil {
		t.Fatalf("ensureBinary() error = %v", err)
	}
	entries, err := os.ReadDir(binDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "sortie" {
		t.Fatalf("binary directory holds %v, want only sortie", entries)
	}
	assertSameContent(t, source, filepath.Join(binDir, "sortie"))
	info, err := os.Stat(filepath.Join(binDir, "sortie"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("copy mode = %v, want 0755", info.Mode().Perm())
	}
}

func TestEnsureBinary_RefreshesAChangedSource(t *testing.T) {
	t.Parallel()
	source := copyOfTestBinary(t)
	binDir := t.TempDir()
	env := binaryEnv(source)
	if err := env.ensureBinary(binDir); err != nil {
		t.Fatalf("ensureBinary() error = %v", err)
	}

	// A new build at the same path: appended bytes keep it a valid ELF
	// file, and the size changes.
	f, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // G304: the test's own copy
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("new build"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := os.Chtimes(source, time.Now(), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := env.ensureBinary(binDir); err != nil {
		t.Fatalf("ensureBinary() after a rebuild error = %v", err)
	}
	assertSameContent(t, source, filepath.Join(binDir, "sortie"))

	// A copy changed behind Sortie's back is replaced once the source's
	// identity changes again.
	if err := os.WriteFile(filepath.Join(binDir, "sortie"), []byte("tampered"), 0o755); err != nil { //nolint:gosec // G306: test
		t.Fatal(err)
	}
	if err := os.Chtimes(source, time.Now(), time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := env.ensureBinary(binDir); err != nil {
		t.Fatalf("ensureBinary() after tampering error = %v", err)
	}
	assertSameContent(t, source, filepath.Join(binDir, "sortie"))
}

func TestEnsureBinary_RefusesANonLinuxExecutable(t *testing.T) {
	t.Parallel()
	source := filepath.Join(t.TempDir(), "sortie")
	if err := os.WriteFile(source, []byte("#!/bin/sh\necho not elf\n"), 0o755); err != nil { //nolint:gosec // G306: test
		t.Fatal(err)
	}
	binDir := t.TempDir()
	err := binaryEnv(source).ensureBinary(binDir)
	if err == nil || !strings.Contains(err.Error(), "not a Linux executable") {
		t.Fatalf("ensureBinary(script) error = %v, want a not-a-Linux-executable error", err)
	}
	if entries, _ := os.ReadDir(binDir); len(entries) != 0 {
		t.Errorf("binary directory holds %v after a refusal, want nothing", entries)
	}
}

func TestPrepare_PlacesTheSortieBinary(t *testing.T) {
	t.Parallel()
	source := copyOfTestBinary(t)
	f := newPrepareFixture(t, fakeState{}, nil)
	f.env.sortieBinary = source
	if _, err := f.env.Prepare(context.Background(), f.ws); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	assertSameContent(t, source, filepath.Join(f.stateDir, "env-bin", "sortie"))
	if GuestSortieBinary != "/opt/sortie/sortie" || f.env.GuestSortieBinaryPath() != GuestSortieBinary {
		t.Errorf("guest binary path = %q", f.env.GuestSortieBinaryPath())
	}
}

func assertSameContent(t *testing.T, want, got string) {
	t.Helper()
	a, err := os.ReadFile(want) //nolint:gosec // G304: test paths
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(got) //nolint:gosec // G304: test paths
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("%s differs from %s", got, want)
	}
}

package smolvm

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// GuestSortieBinary is the path, in the VM, of the copy of the Sortie
// binary the VM runs as its tool relay.
const GuestSortieBinary = GuestBinDir + "/" + binaryName

const binaryName = "sortie"

// elfMachines maps the architectures Sortie builds for to their ELF
// machine. The guest has the host's architecture, so the copy must be
// built for it.
var elfMachines = map[string]elf.Machine{
	"amd64": elf.EM_X86_64,
	"arm64": elf.EM_AARCH64,
}

// binarySource identifies the Sortie binary the binary directory last
// received a copy of.
type binarySource struct {
	path    string
	size    int64
	modTime time.Time
}

// ensureBinary makes the binary directory under the state directory
// hold exactly one file, a copy of the Sortie binary, mode 0755, that
// matches the source binary: the configured one, or the running
// executable. A copy whose content differs is replaced atomically. The
// source must be a Linux executable for the host's architecture,
// because the guest runs it as it is; a dynamically linked one is
// copied with a warning, since the guest may lack its libraries.
func (e *Environment) ensureBinary(binDir string) error {
	named := e.sortieBinary
	if named == "" {
		running, err := os.Executable()
		if err != nil {
			return fmt.Errorf("smolvm environment: resolve the running Sortie binary: %w", err)
		}
		named = running
	}
	resolved, err := filepath.EvalSymlinks(named)
	if err != nil {
		return fmt.Errorf("smolvm environment: resolve the Sortie binary %q: %w", named, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("smolvm environment: the Sortie binary %q: %w", resolved, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("smolvm environment: the Sortie binary %q is not a regular file", resolved)
	}
	source := binarySource{path: resolved, size: info.Size(), modTime: info.ModTime()}
	dest := filepath.Join(binDir, binaryName)

	e.binMu.Lock()
	defer e.binMu.Unlock()
	if e.binSource == source && sameSize(dest, source.size) {
		return removeOthers(binDir)
	}
	dynamic, err := checkGuestExecutable(resolved)
	if err != nil {
		return err
	}
	if dynamic {
		e.logger.Warn("the Sortie binary is dynamically linked; the VM may be unable to run it, and the agent's Sortie tools would then fail. Build Sortie with CGO_ENABLED=0",
			slog.String("binary", resolved))
	}
	upToDate, err := copyIsCurrent(resolved, dest, source.size)
	if err != nil {
		return err
	}
	if !upToDate {
		if err := copyBinary(resolved, binDir, dest); err != nil {
			return err
		}
	} else if err := os.Chmod(dest, 0o755); err != nil { //nolint:gosec // G302: the VM executes it
		return fmt.Errorf("smolvm environment: set the Sortie binary copy's mode: %w", err)
	}
	if err := removeOthers(binDir); err != nil {
		return err
	}
	e.binSource = source
	return nil
}

// copyIsCurrent reports whether dest is a regular file with source's
// content. Only a copy of the same size is read and compared.
func copyIsCurrent(source, dest string, size int64) (bool, error) {
	if !sameSize(dest, size) {
		return false, nil
	}
	want, err := fileDigest(source)
	if err != nil {
		return false, fmt.Errorf("smolvm environment: read the Sortie binary: %w", err)
	}
	have, err := fileDigest(dest)
	if err != nil {
		return false, nil
	}
	return bytes.Equal(have, want), nil
}

// checkGuestExecutable fails unless path is a Linux ELF executable for
// this host's architecture, and reports whether it is dynamically
// linked.
func checkGuestExecutable(path string) (dynamic bool, err error) {
	if runtime.GOOS != "linux" {
		return false, fmt.Errorf("smolvm environment: the VM runs this host's Sortie binary, which only a Linux host has (host is %s)", runtime.GOOS)
	}
	f, err := elf.Open(path)
	if err != nil {
		return false, fmt.Errorf("smolvm environment: the Sortie binary %q is not a Linux executable: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only file
	if want, ok := elfMachines[runtime.GOARCH]; ok && f.Machine != want {
		return false, fmt.Errorf("smolvm environment: the Sortie binary %q is built for %s, not this host's %s", path, f.Machine, runtime.GOARCH)
	}
	for _, prog := range f.Progs {
		if prog.Type == elf.PT_INTERP {
			return true, nil
		}
	}
	return false, nil
}

func sameSize(path string, size int64) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() == size
}

func fileDigest(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: Sortie's own binary or its copy
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// copyBinary writes a copy of source to a temporary file in binDir and
// renames it over dest, so a VM never sees a partial binary.
func copyBinary(source, binDir, dest string) error {
	in, err := os.Open(source) //nolint:gosec // G304: Sortie's own binary
	if err != nil {
		return fmt.Errorf("smolvm environment: read the Sortie binary: %w", err)
	}
	defer in.Close() //nolint:errcheck // read-only file
	tmp, err := os.CreateTemp(binDir, "."+binaryName+".*")
	if err != nil {
		return fmt.Errorf("smolvm environment: copy the Sortie binary: %w", err)
	}
	_, copyErr := io.Copy(tmp, in)
	chmodErr := tmp.Chmod(0o755)
	closeErr := tmp.Close()
	if err := errors.Join(copyErr, chmodErr, closeErr); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("smolvm environment: copy the Sortie binary: %w", err)
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("smolvm environment: copy the Sortie binary: %w", err)
	}
	return nil
}

// removeOthers removes every entry of binDir but the binary, so the
// directory every VM mounts holds nothing else.
func removeOthers(binDir string) error {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return fmt.Errorf("smolvm environment: list the binary directory: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == binaryName && entry.Type().IsRegular() {
			continue
		}
		if err := os.RemoveAll(filepath.Join(binDir, entry.Name())); err != nil {
			return fmt.Errorf("smolvm environment: clear the binary directory: %w", err)
		}
	}
	return nil
}

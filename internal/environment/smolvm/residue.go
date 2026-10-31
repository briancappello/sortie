package smolvm

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// machineCacheDir returns the directory where smolvm keeps a directory
// per machine, as it resolves it from the environment it inherits from
// Sortie: $XDG_CACHE_HOME/smolvm/vms when that is absolute, else
// ~/.cache/smolvm/vms. It returns "" when neither can be resolved.
func machineCacheDir() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "smolvm", "vms")
}

// machineResidue returns the entries smolvm leaves under dir when a
// command addresses the machine name that does not exist: a directory
// named for the first 16 hex digits of the name's SHA-256, which exec
// and stop leave empty, and the lock file .<name>.fork-operation.lock,
// which stop and delete leave empty.
func machineResidue(dir, name string) (machineDir, lock string) {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(dir, hex.EncodeToString(sum[:])[:16]),
		filepath.Join(dir, "."+name+".fork-operation.lock")
}

// removeMissingMachineResidue removes what smolvm left for name, a
// machine that does not exist: its machine directory when it is empty,
// and its lock file when it is an empty regular file. Anything else at
// those paths is left alone, so a machine that exists, or that another
// process creates meanwhile, keeps its files. Failures are ignored:
// the residue is clutter, not state.
func (e *Environment) removeMissingMachineResidue(name string) {
	if e.machineCache == "" {
		return
	}
	machineDir, lock := machineResidue(e.machineCache, name)
	if info, err := os.Lstat(lock); err == nil && info.Mode().IsRegular() && info.Size() == 0 {
		_ = os.Remove(lock)
	}
	// os.Remove removes a directory only when it is empty.
	if info, err := os.Lstat(machineDir); err == nil && info.IsDir() {
		if err := os.Remove(machineDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			e.logger.Debug("left smolvm's directory of a missing machine in place", slog.String("vm", name), slog.Any("error", err))
		}
	}
}

//go:build unix

package agenttest

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// HostPrefixVersion is what the stand-in host prefix [WriteHostPrefix]
// writes prints for a run whose only argument is --version.
const HostPrefixVersion = "0.0.0-host-prefix"

// HostPrefixRun is one run of the stand-in host prefix [WriteHostPrefix]
// writes.
type HostPrefixRun struct {
	// Program is the path the run was started as.
	Program string

	// Args are the run's arguments: the rest of the prefix, then the
	// launch it runs.
	Args []string

	// Env holds the value of every variable WriteHostPrefix was asked to
	// record that was set in the run's environment.
	Env map[string]string
}

// WriteHostPrefix writes an executable shell script to dir/name that
// stands in for a host prefix program, and returns its path and a
// function reading the runs it has recorded so far, in order. Each run
// records its program path, its arguments, and the values of the names
// in envNames it finds set, then execs its arguments after a leading
// "--", which it takes as its own separator, so the launch
// behind it runs as that process and its exit status, standard input,
// and output are the prefix's. A run whose only argument is --version
// prints [HostPrefixVersion] instead, as a wrapper that answers a
// version query of its own would.
func WriteHostPrefix(t *testing.T, dir, name string, envNames ...string) (string, func() []HostPrefixRun) {
	t.Helper()
	logPath := filepath.Join(dir, name+".runs")
	quoted := make([]string, 0, len(envNames))
	for _, n := range envNames {
		quoted = append(quoted, shellWord(n))
	}
	script := "{\n" +
		"  printf 'run\\0%s\\0' \"$0\"\n" +
		"  for name in " + strings.Join(quoted, " ") + "; do\n" +
		"    if value=$(printenv \"$name\"); then printf '%s=%s\\0' \"$name\" \"$value\"; fi\n" +
		"  done\n" +
		"  printf 'args\\0%s\\0' \"$#\"\n" +
		"  for arg in \"$@\"; do printf '%s\\0' \"$arg\"; done\n" +
		"} >> " + shellWord(logPath) + "\n" +
		"if [ \"$#\" -eq 1 ] && [ \"$1\" = --version ]; then echo " + shellWord(HostPrefixVersion) + "; exit 0; fi\n" +
		"if [ \"$1\" = -- ]; then shift; fi\n" +
		"exec \"$@\"\n"
	path := WriteScript(t, dir, name, script)
	return path, func() []HostPrefixRun {
		t.Helper()
		return readHostPrefixRuns(t, logPath)
	}
}

func readHostPrefixRuns(t *testing.T, logPath string) []HostPrefixRun {
	t.Helper()
	data, err := os.ReadFile(logPath) //nolint:gosec // the test names the log its own prefix writes
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read host prefix log: %v", err)
	}
	if !bytes.HasSuffix(data, []byte{0}) {
		if len(data) != 0 {
			t.Fatalf("host prefix log %s ends inside a field", logPath)
		}
		return nil
	}
	fields := strings.Split(string(data[:len(data)-1]), "\x00")
	var runs []HostPrefixRun
	for i := 0; i < len(fields); {
		if fields[i] != "run" || i+1 >= len(fields) {
			t.Fatalf("host prefix log %s: field %d = %q, want the start of a run", logPath, i, fields[i])
		}
		run := HostPrefixRun{Program: fields[i+1], Env: map[string]string{}}
		i += 2
		for i < len(fields) && fields[i] != "args" {
			name, value, _ := strings.Cut(fields[i], "=")
			run.Env[name] = value
			i++
		}
		if i+1 >= len(fields) {
			t.Fatalf("host prefix log %s: a run ends before its arguments", logPath)
		}
		n, convErr := strconv.Atoi(fields[i+1])
		if convErr != nil || i+2+n > len(fields) {
			t.Fatalf("host prefix log %s: argument count %q does not fit the log", logPath, fields[i+1])
		}
		run.Args = append([]string{}, fields[i+2:i+2+n]...)
		i += 2 + n
		runs = append(runs, run)
	}
	return runs
}

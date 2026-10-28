//go:build unix

package toolsocket

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helperEnv makes the test binary act as a tool server: it answers each
// input line with its SORTIE_ISSUE_ID and the line, and exits at the end
// of its input. With helperHang set, it ignores its input and sleeps.
const (
	helperEnv  = "TOOLSOCKET_TEST_HELPER"
	helperHang = "TOOLSOCKET_TEST_HANG"
	helperPID  = "TOOLSOCKET_TEST_PIDFILE"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		runHelper()
		return
	}
	os.Exit(m.Run())
}

func runHelper() {
	if path := os.Getenv(helperPID); path != "" {
		_ = os.WriteFile(path, fmt.Appendf(nil, "%d", os.Getpid()), 0o600)
	}
	if os.Getenv(helperHang) == "1" {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		fmt.Printf("%s:%s\n", os.Getenv("SORTIE_ISSUE_ID"), sc.Text())
	}
	if sc.Err() != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func helperSpec(t *testing.T, env ...string) Spec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return Spec{Argv: []string{exe}, Env: append([]string{helperEnv + "=1"}, env...)}
}

// socketPath returns a socket path short enough for the Unix socket
// address limit, in a fresh directory.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ts")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "k", "tools.sock")
}

func newRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry(nil)
	r.checkEvery = 20 * time.Millisecond
	r.exitGrace = 200 * time.Millisecond
	t.Cleanup(r.Close)
	return r
}

// call connects to path, sends line, ends its input, and returns
// everything the server wrote.
func call(t *testing.T, path, line string) string {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("Dial(%s): %v", path, err)
	}
	defer conn.Close() //nolint:errcheck // test
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.(*net.UnixConn).CloseWrite()
	out, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(out)
}

func TestServe_EachSocketServesItsOwnIdentity(t *testing.T) {
	t.Parallel()
	r := newRegistry(t)
	p1, p2 := socketPath(t), socketPath(t)
	if err := r.Serve("VM-1", p1, helperSpec(t, "SORTIE_ISSUE_ID=id-1")); err != nil {
		t.Fatalf("Serve(VM-1): %v", err)
	}
	if err := r.Serve("VM-2", p2, helperSpec(t, "SORTIE_ISSUE_ID=id-2")); err != nil {
		t.Fatalf("Serve(VM-2): %v", err)
	}
	if got := call(t, p2, "status"); got != "id-2:status\n" {
		t.Errorf("VM-2 socket answered %q, want %q", got, "id-2:status\n")
	}
	if got := call(t, p1, "status"); got != "id-1:status\n" {
		t.Errorf("VM-1 socket answered %q, want %q", got, "id-1:status\n")
	}
}

func TestServe_SocketMode(t *testing.T) {
	t.Parallel()
	r := newRegistry(t)
	p := socketPath(t)
	if err := r.Serve("k", p, helperSpec(t)); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode().Type() != os.ModeSocket || info.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want a socket with 0600", info.Mode())
	}
}

func TestServe_UpdatesSpecForLaterConnections(t *testing.T) {
	t.Parallel()
	r := newRegistry(t)
	p := socketPath(t)
	if err := r.Serve("k", p, helperSpec(t, "SORTIE_ISSUE_ID=first")); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	before, _ := os.Lstat(p)
	if err := r.Serve("k", p, helperSpec(t, "SORTIE_ISSUE_ID=second")); err != nil {
		t.Fatalf("Serve again: %v", err)
	}
	after, _ := os.Lstat(p)
	if !os.SameFile(before, after) {
		t.Error("serving a new spec rebound the socket, want the same socket kept")
	}
	if got := call(t, p, "x"); got != "second:x\n" {
		t.Errorf("answer = %q, want %q", got, "second:x\n")
	}
}

func TestServe_RestartedAtSamePathServesNewConnection(t *testing.T) {
	t.Parallel()
	p := socketPath(t)

	first := NewRegistry(nil)
	if err := first.Serve("k", p, helperSpec(t, "SORTIE_ISSUE_ID=a")); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if got := call(t, p, "1"); got != "a:1\n" {
		t.Fatalf("first answer = %q", got)
	}
	// A crash leaves the socket file behind: close the listener without
	// letting it remove the file.
	first.mu.Lock()
	l := first.listeners["k"]
	first.mu.Unlock()
	_ = l.ln.Close()
	if _, err := os.Lstat(p); err != nil {
		t.Fatalf("stale socket file missing: %v", err)
	}
	// Close returns before the kernel socket is closed: that happens when
	// the goroutine blocked in Accept drops its reference, which can lag
	// under load. Wait for the refusal instead of expecting it at once.
	refused := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		conn, err := net.Dial("unix", p)
		if err != nil {
			refused = true
			break
		}
		_ = conn.Close()
	}
	if !refused {
		t.Fatal("Dial still succeeds on a stale socket, want refused")
	}

	second := newRegistry(t)
	if err := second.Serve("k", p, helperSpec(t, "SORTIE_ISSUE_ID=b")); err != nil {
		t.Fatalf("Serve after restart: %v", err)
	}
	if got := call(t, p, "2"); got != "b:2\n" {
		t.Errorf("answer after restart = %q, want %q", got, "b:2\n")
	}
	first.Close()
	// Closing the old registry must not remove the new listener's file.
	if got := call(t, p, "3"); got != "b:3\n" {
		t.Errorf("answer after the old registry closed = %q, want %q", got, "b:3\n")
	}
}

func TestStop_ClosesAndRemovesSocket(t *testing.T) {
	t.Parallel()
	r := newRegistry(t)
	p := socketPath(t)
	if err := r.Serve("k", p, helperSpec(t)); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	r.Stop("k")
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Errorf("socket file after Stop: err = %v, want not exist", err)
	}
	if r.Serving("k") {
		t.Error("Serving(k) after Stop = true")
	}
}

func TestServe_StopsWhenSocketDirectoryRemoved(t *testing.T) {
	t.Parallel()
	r := newRegistry(t)
	p := socketPath(t)
	if err := r.Serve("k", p, helperSpec(t)); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if err := os.RemoveAll(filepath.Dir(p)); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.Serving("k") {
		if time.Now().After(deadline) {
			t.Fatal("listener still registered after its socket directory was removed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A new listener at the same path, as a recreated workspace gets.
	if err := r.Serve("k", p, helperSpec(t, "SORTIE_ISSUE_ID=new")); err != nil {
		t.Fatalf("Serve after removal: %v", err)
	}
	if got := call(t, p, "x"); got != "new:x\n" {
		t.Errorf("answer = %q, want %q", got, "new:x\n")
	}
}

func TestServe_ReplacesListenerWhoseSocketWasReplaced(t *testing.T) {
	t.Parallel()
	r := NewRegistry(nil) // the watcher must not get there first
	r.checkEvery = time.Hour
	t.Cleanup(r.Close)
	p := socketPath(t)
	if err := r.Serve("k", p, helperSpec(t, "SORTIE_ISSUE_ID=a")); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := r.Serve("k", p, helperSpec(t, "SORTIE_ISSUE_ID=b")); err != nil {
		t.Fatalf("Serve after the file was removed: %v", err)
	}
	if got := call(t, p, "x"); got != "b:x\n" {
		t.Errorf("answer = %q, want %q", got, "b:x\n")
	}
}

func TestServe_RefusesNonSocketPath(t *testing.T) {
	t.Parallel()
	r := newRegistry(t)
	p := socketPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Serve("k", p, helperSpec(t)); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("Serve over a regular file: err = %v, want a not-a-socket error", err)
	}
	if data, _ := os.ReadFile(p); string(data) != "keep" {
		t.Errorf("regular file was changed to %q", data)
	}
}

func TestServe_RefusesRelativeCommand(t *testing.T) {
	t.Parallel()
	r := newRegistry(t)
	if err := r.Serve("k", socketPath(t), Spec{Argv: []string{"sortie"}}); err == nil {
		t.Fatal("Serve with a relative command succeeded")
	}
}

func TestHandle_KillsProcessWhenConnectionCloses(t *testing.T) {
	t.Parallel()
	r := newRegistry(t)
	p := socketPath(t)
	pidFile := filepath.Join(filepath.Dir(filepath.Dir(p)), "pid")
	if err := r.Serve("k", p, helperSpec(t, helperHang+"=1", helperPID+"="+pidFile)); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	conn, err := net.Dial("unix", p)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(pidFile)
		if _, err := fmt.Sscanf(string(data), "%d", &pid); err == nil && pid > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The process ignores the end of its input; it is killed after the
	// grace period.
	_ = conn.Close()

	r.mu.Lock()
	l := r.listeners["k"]
	r.mu.Unlock()
	deadline = time.Now().Add(10 * time.Second)
	for {
		l.mu.Lock()
		open := len(l.conns)
		l.mu.Unlock()
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("connection handler still running after the client went away")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The handler returned after Wait, so the process was reaped.
	if err := processAlive(pid); err == nil {
		t.Errorf("process %d still exists after its connection closed", pid)
	}
}

func processAlive(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.Signal(0))
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// relaySocketPath returns a socket path short enough for the Unix
// socket address limit.
func relaySocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "relay")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "tools.sock")
}

func listenRelay(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	path := relaySocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(conn)
		}
	}()
	return path
}

func runRelayForTest(t *testing.T, path string, stdin io.Reader) (int, string, string) {
	t.Helper()
	return runRelayWithTimeout(t, path, stdin, mcpRelayFirstResponseTimeout)
}

func runRelayWithTimeout(t *testing.T, path string, stdin io.Reader, firstResponse time.Duration) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- relayMCP(context.Background(), path, stdin, &stdout, &stderr, firstResponse) }()
	select {
	case code := <-done:
		return code, stdout.String(), stderr.String()
	case <-time.After(10 * time.Second):
		t.Fatal("relayMCP did not return")
		return 0, "", ""
	}
}

func TestMCPRelay_ListeningSocket(t *testing.T) {
	t.Parallel()

	path := listenRelay(t, func(conn net.Conn) {
		defer conn.Close() //nolint:errcheck // test server
		r := bufio.NewReader(conn)
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		_, _ = io.WriteString(conn, "echo:"+line)
		// The relay half-closes once its stdin ends; drain to that EOF
		// before answering a second time, so this also proves the
		// relay keeps reading after its stdin is done.
		rest, _ := io.ReadAll(r)
		_, _ = io.WriteString(conn, "rest:"+string(rest)+"\n")
	})

	code, stdout, stderr := runRelayForTest(t, path, strings.NewReader("hello\nworld\n"))
	if code != 0 {
		t.Fatalf("runMCPRelay() = %d, want 0; stderr %q", code, stderr)
	}
	if want := "echo:hello\nrest:world\n\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestMCPRelay_SocketClosesImmediately(t *testing.T) {
	t.Parallel()

	path := listenRelay(t, func(conn net.Conn) { _ = conn.Close() })

	code, stdout, stderr := runRelayForTest(t, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`+"\n"))
	if code == 0 {
		t.Fatal("runMCPRelay() = 0, want non-zero")
	}
	if !strings.Contains(stderr, "tool server unavailable") {
		t.Errorf("stderr = %q, want it to contain %q", stderr, "tool server unavailable")
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
}

func TestMCPRelay_MissingSocket(t *testing.T) {
	t.Parallel()

	path := relaySocketPath(t)

	code, _, stderr := runRelayForTest(t, path, strings.NewReader("x\n"))
	if code == 0 {
		t.Fatal("runMCPRelay() = 0, want non-zero")
	}
	if !strings.Contains(stderr, "tool server unavailable") {
		t.Errorf("stderr = %q, want it to contain %q", stderr, "tool server unavailable")
	}
}

func TestMCPRelay_AcceptsButNeverAnswers(t *testing.T) {
	t.Parallel()

	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	path := listenRelay(t, func(conn net.Conn) {
		defer conn.Close() //nolint:errcheck // test server
		<-hold
	})

	// The client sends its request and keeps stdin open, as an MCP
	// client waiting for the initialize response does.
	stdinR, stdinW := io.Pipe()
	t.Cleanup(func() { _ = stdinW.Close() })
	go func() { _, _ = io.WriteString(stdinW, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`+"\n") }()

	code, _, stderr := runRelayWithTimeout(t, path, stdinR, 200*time.Millisecond)
	if code == 0 {
		t.Fatal("runMCPRelay() = 0, want non-zero")
	}
	if !strings.Contains(stderr, "tool server unavailable") {
		t.Errorf("stderr = %q, want it to contain %q", stderr, "tool server unavailable")
	}
}

func TestMCPRelay_RequiresOneSocket(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := runMCPRelay(context.Background(), nil, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatal("runMCPRelay(no args) = 0, want non-zero")
	}
	if code := runMCPRelay(context.Background(), []string{"a", "b"}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatal("runMCPRelay(two args) = 0, want non-zero")
	}
}

func TestPrintMCPRelayHelp(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	printMCPRelayHelp(&buf)

	golden, err := os.ReadFile("testdata/help-mcp-relay.txt")
	if err != nil {
		t.Fatalf("os.ReadFile(%q): %v", "testdata/help-mcp-relay.txt", err)
	}
	if got := buf.String(); got != string(golden) {
		t.Errorf("printMCPRelayHelp() output mismatch\ngot:\n%s\nwant:\n%s", got, golden)
	}
}

func TestBoot_DispatchesMCPRelay(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	_, code := boot(context.Background(), bootParams{args: []string{"mcp-relay", "-h"}, stdout: &stdout, stderr: &stderr})
	if code != 0 {
		t.Fatalf("boot(mcp-relay -h) = %d, want 0; stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "sortie mcp-relay SOCKET") {
		t.Errorf("stdout = %q, want the mcp-relay help", stdout.String())
	}
}

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// mcpRelayUnavailable is the message mcp-relay writes to standard error
// when the socket gives no response: nothing listens behind it, or the
// tool server closed the connection before answering.
const mcpRelayUnavailable = "tool server unavailable"

// mcpRelayFirstResponseTimeout bounds how long mcp-relay waits for the
// tool server's first response byte once the client has sent a request.
// A socket that accepts but never answers must not hang the agent.
const mcpRelayFirstResponseTimeout = 30 * time.Second

func runMCPRelay(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	if containsHelpFlag(args) {
		printMCPRelayHelp(stdout)
		return 0
	}
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(stderr, "sortie mcp-relay: exactly one socket path is required") //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}
	return relayMCP(ctx, args[0], stdin, stdout, stderr, mcpRelayFirstResponseTimeout)
}

// relayMCP connects to the Unix socket at path and copies stdin to it and
// its output to stdout. It fails with [mcpRelayUnavailable] when the
// connection gives no response byte: a connect error, an end of stream, a
// read error, or no answer within firstResponse of the first request byte.
// After the first response byte it is a transparent pipe; the end of stdin
// half-closes the connection, and the relay exits 0 when the server then
// closes it.
func relayMCP(ctx context.Context, path string, stdin io.Reader, stdout io.Writer, stderr io.Writer, firstResponse time.Duration) int {
	var dialer net.Dialer
	raw, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		fmt.Fprintf(stderr, "sortie mcp-relay: %s: %v\n", mcpRelayUnavailable, err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}
	conn := raw.(*net.UnixConn)
	defer conn.Close() //nolint:errcheck // nothing to report once the relay ends

	// Closing the connection on cancellation unblocks both copies.
	stopOnCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopOnCancel()

	// sent is closed once the first request bytes reach the socket; the
	// first-response deadline starts then, because an MCP client speaks
	// first and a server is not expected to answer before it does.
	sent := make(chan struct{})
	go func() {
		w := &firstWriteSignal{w: conn, signal: sent}
		_, _ = io.Copy(w, stdin)
		// The client is done sending; let the server see EOF and finish.
		_ = conn.CloseWrite()
	}()

	gotFirst := make(chan struct{})
	go func() {
		select {
		case <-sent:
		case <-gotFirst:
			return
		}
		timer := time.NewTimer(firstResponse)
		defer timer.Stop()
		select {
		case <-timer.C:
			_ = conn.SetReadDeadline(time.Now())
		case <-gotFirst:
		}
	}()

	buf := make([]byte, 32*1024)
	n, err := conn.Read(buf)
	close(gotFirst)
	if n == 0 {
		if ctx.Err() != nil {
			return 1
		}
		detail := "connection closed before any response"
		if err != nil && !errors.Is(err, io.EOF) {
			detail = err.Error()
		}
		fmt.Fprintf(stderr, "sortie mcp-relay: %s: %s\n", mcpRelayUnavailable, detail) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}
	_ = conn.SetReadDeadline(time.Time{})

	if _, werr := stdout.Write(buf[:n]); werr != nil {
		return 1
	}
	if err == nil {
		_, err = io.Copy(stdout, conn)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		if ctx.Err() != nil {
			return 1
		}
		fmt.Fprintf(stderr, "sortie mcp-relay: %v\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}
	return 0
}

// firstWriteSignal closes signal after the first successful write that
// carries bytes.
type firstWriteSignal struct {
	w      io.Writer
	signal chan struct{}
	done   bool
}

func (f *firstWriteSignal) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if n > 0 && !f.done {
		f.done = true
		close(f.signal)
	}
	return n, err
}

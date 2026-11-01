package dataplane

import (
	"context"
	"fmt"
	"io"
	"net"
)

// BridgeCommand is the subcommand of Sortie's binary that runs
// [RunBridge]: `sortie stdio-bridge <socket>`.
const BridgeCommand = "stdio-bridge"

// RunBridge joins this process's standard streams to the agent session of
// one remote job, through the session socket an [Attempt] listens on. It
// is the host process an agent adapter launches in place of the agent:
// what the adapter writes to its standard input reaches the agent's, and
// what the agent writes to its standard output and error comes back on
// stdout and stderr. It returns the agent's exit status once the worker
// reports it. While the worker's stream is broken it stays silent, as a
// stalled agent does, until it is stopped.
func RunBridge(socket string, stdin io.Reader, stdout, stderr io.Writer) int {
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "unix", socket)
	if err != nil {
		fmt.Fprintf(stderr, "sortie %s: %v\n", BridgeCommand, err) //nolint:errcheck // nowhere else to report
		return 1
	}
	defer conn.Close() //nolint:errcheck // the process ends next

	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, readErr := stdin.Read(buf)
			if n > 0 {
				if WriteFrame(conn, FrameStdin, buf[:n]) != nil {
					return
				}
			}
			if readErr != nil {
				_ = WriteFrame(conn, FrameStdinClosed, nil) //nolint:errcheck // the session ends either way
				return
			}
		}
	}()

	for {
		typ, payload, err := ReadFrame(conn)
		if err != nil {
			fmt.Fprintf(stderr, "sortie %s: the session ended without the agent's exit: %v\n", BridgeCommand, err) //nolint:errcheck // nowhere else to report
			return 1
		}
		switch typ {
		case FrameStdout:
			if _, err := stdout.Write(payload); err != nil {
				return 1
			}
		case FrameStderr:
			_, _ = stderr.Write(payload) //nolint:errcheck // diagnostics only
		case FrameExit:
			status, err := ExitStatus(payload)
			if err != nil {
				return 1
			}
			return status
		}
	}
}

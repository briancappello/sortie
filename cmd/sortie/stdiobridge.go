package main

import (
	"fmt"
	"io"
	"os"

	"github.com/sortie-ai/sortie/internal/dataplane"
)

// runStdioBridge runs `sortie stdio-bridge <socket>`: the host process
// an agent session of a remote job is launched as, whose standard
// streams the job's stdio flow carries to the agent on the worker.
func runStdioBridge(args []string, stderr io.Writer) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintf(stderr, "usage: sortie %s <socket>\n", dataplane.BridgeCommand) //nolint:errcheck // stderr write failure is unrecoverable
		return 2
	}
	return dataplane.RunBridge(args[0], os.Stdin, os.Stdout, stderr)
}

package agenttest

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"slices"

	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
)

// HTTPToolServerScenario names the built-in [Scenario] that stands in for
// "sortie mcp-server --http": it listens on a loopback port, announces it on
// standard output as the real server does, answers every request that
// presents the bearer the relay hands it with [HTTPToolServerReply], and
// reports each such request as a connection. It serves until its standard
// input closes.
const HTTPToolServerScenario = "agenttest.httptoolserver"

// HTTPToolServerReply is the body [HTTPToolServerScenario] answers with.
const HTTPToolServerReply = "served by the stand-in tool server"

func runHTTPToolServer(args []string, _ struct{}) int {
	if !slices.Contains(args, "--http") {
		return 2
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return 3
	}
	bearer := os.Getenv(mcpconfig.BearerEnv)
	out := json.NewEncoder(os.Stdout)
	if err := out.Encode(map[string]any{"listening": listener.Addr().String()}); err != nil {
		return 3
	}
	go func() {
		_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { //nolint:gosec // a loopback test stand-in with no timeouts to tune
			if r.Header.Get("Authorization") != "Bearer "+bearer {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = out.Encode(map[string]any{"connected": true})
			_, _ = io.WriteString(w, HTTPToolServerReply)
		}))
	}()
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

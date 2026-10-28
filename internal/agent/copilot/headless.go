package copilot

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

const (
	headlessConnectID = 1
	headlessQuotaID   = 2
)

var headlessArgs = []string{"--headless", "--no-auto-update", "--stdio"}

type headlessRequest struct {
	JSONRPC string   `json:"jsonrpc"`
	ID      int      `json:"id"`
	Method  string   `json:"method"`
	Params  struct{} `json:"params"`
}

type headlessMessage struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct{}       `json:"error"`
}

type headlessQuotaAnswer struct {
	QuotaSnapshots map[string]headlessQuotaSnapshot `json:"quotaSnapshots"`
}

type headlessQuotaSnapshot struct {
	IsUnlimitedEntitlement bool     `json:"isUnlimitedEntitlement"`
	RemainingPercentage    *float64 `json:"remainingPercentage"`
	ResetDate              string   `json:"resetDate"`
}

// headlessQuota asks a one-shot copilot server for the account's quota
// windows over JSON-RPC on stdio, with the runtime's own sign-in. It returns
// one reading per limited quota; an error answer from the server wraps
// [usage.ErrPullUnsupported].
func headlessQuota(ctx context.Context, target agentcore.LaunchTarget, env []string) ([]usage.WindowReading, error) {
	runCtx, stopProcess := context.WithCancel(ctx)
	defer stopProcess()

	stdinReader, stdin := io.Pipe()
	defer func() { _ = stdinReader.Close() }() //nolint:errcheck // unblocks a pending write once the server is gone

	cmd, agentErr := target.AuxiliaryCommand(runCtx, headlessArgs, stdinReader, env)
	if agentErr != nil {
		return nil, fmt.Errorf("build copilot headless command: %w", agentErr)
	}
	logger := slog.Default()
	procutil.SetGroupCancel(cmd, target.StopGrace)
	pipes, _, err := procutil.StartWithOwnedPipes(runCtx, cmd, logger)
	if err != nil {
		return nil, fmt.Errorf("start copilot headless server: %w", err)
	}
	defer pipes.Close() //nolint:errcheck,gosec // best-effort cleanup; runs after the reap wait below

	// Stderr is drained and dropped: the server runs with the turn's credentials.
	go func() { _, _ = io.Copy(io.Discard, pipes.Stderr) }() //nolint:errcheck // ends when pipes close

	reaper := procutil.StartReaper(cmd, logger)
	defer func() { <-reaper.Done() }()
	exchangeDone := make(chan struct{})
	procutil.StartOutputRelease(procutil.OutputReleaseParams{
		Pipes:      pipes,
		Reaped:     reaper.Done(),
		ReaderDone: exchangeDone,
		Logger:     logger,
	})
	defer func() {
		_ = stdin.Close() //nolint:errcheck // the exchange is over or abandoned
		stopProcess()
	}()

	type outcome struct {
		readings []usage.WindowReading
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		defer close(exchangeDone)
		readings, err := headlessExchange(stdin, pipes.Stdout)
		done <- outcome{readings: readings, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("query copilot quota: %w", ctx.Err())
	case res := <-done:
		return res.readings, res.err
	}
}

func headlessExchange(stdin io.Writer, stdout io.Reader) ([]usage.WindowReading, error) {
	br := bufio.NewReader(stdout)
	if _, err := headlessCall(stdin, br, headlessConnectID, "connect"); err != nil {
		return nil, err
	}
	result, err := headlessCall(stdin, br, headlessQuotaID, "account.getQuota")
	if err != nil {
		return nil, err
	}
	return parseHeadlessQuota(result)
}

// headlessCall sends one request with empty params and returns the result
// of the response carrying id. Messages that are not that response, such
// as notifications, are skipped.
func headlessCall(stdin io.Writer, stdout *bufio.Reader, id int, method string) (json.RawMessage, error) {
	payload, err := json.Marshal(headlessRequest{JSONRPC: "2.0", ID: id, Method: method})
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", method, err)
	}
	if err := writeContentLengthFramed(stdin, payload); err != nil {
		return nil, fmt.Errorf("send %s request: %w", method, err)
	}

	for {
		length, err := readContentLengthHeader(stdout)
		if err != nil {
			return nil, fmt.Errorf("read %s answer: %w", method, err)
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(stdout, body); err != nil {
			return nil, fmt.Errorf("read %s answer: %w", method, err)
		}

		var msg headlessMessage
		if json.Unmarshal(body, &msg) != nil || msg.Method != "" || msg.ID == nil || *msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("%s answered with an error: %w", method, usage.ErrPullUnsupported)
		}
		return msg.Result, nil
	}
}

func parseHeadlessQuota(result json.RawMessage) ([]usage.WindowReading, error) {
	var answer headlessQuotaAnswer
	if err := json.Unmarshal(result, &answer); err != nil {
		return nil, fmt.Errorf("decode account.getQuota answer: %w", err)
	}

	var readings []usage.WindowReading
	for _, key := range slices.Sorted(maps.Keys(answer.QuotaSnapshots)) {
		snapshot := answer.QuotaSnapshots[key]
		if snapshot.IsUnlimitedEntitlement {
			continue
		}
		readings = append(readings, usage.WindowReading{
			Window:     key,
			Share:      snapshot.RemainingPercentage,
			ObservedAt: headlessObservedAt(snapshot.ResetDate),
		})
	}
	return readings, nil
}

// headlessObservedAt reads resetDate as the observation time: the server
// answers from a cached snapshot and stamps it there, so the field is not
// a reset instant.
func headlessObservedAt(resetDate string) int64 {
	instant, err := time.Parse(time.RFC3339, resetDate)
	if err != nil {
		return 0
	}
	return instant.Unix()
}

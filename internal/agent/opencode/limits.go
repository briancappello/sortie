package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

const (
	freeLimitType  = "provider.free-limit"
	limitedStatus  = "limited"
	minutesPerWeek = 7 * 24 * 60
)

// limitsSource declares the console limits the usage command reports.
var limitsSource = usage.Source{
	Strategy: usage.StrategyPull,
	Windows: usage.WindowDeclaration{
		Share:    usage.SharePercent,
		Statuses: map[string]domain.UsageWindowStatus{limitedStatus: domain.UsageWindowExhausted},
		Spans: map[string]usage.Span{
			"rolling": {Period: "Rolling"},
			"weekly":  {Minutes: minutesPerWeek},
			"monthly": {Period: "Monthly"},
		},
	},
}

// freeLimitSource declares the free usage limit a run's error event
// reports.
var freeLimitSource = usage.Source{
	Windows: usage.WindowDeclaration{
		Statuses: map[string]domain.UsageWindowStatus{freeLimitType: domain.UsageWindowExhausted},
		Spans:    map[string]usage.Span{freeLimitType: {Scope: "free usage"}},
	},
}

type rawMeter struct {
	Percent  *float64 `json:"percent"`
	Limited  bool     `json:"limited"`
	ResetsAt int64    `json:"resetsAt"` // Unix milliseconds
}

type rawGoLimits struct {
	Rolling *rawMeter `json:"rolling"`
	Weekly  *rawMeter `json:"weekly"`
	Monthly *rawMeter `json:"monthly"`
}

type rawLimitsBody struct {
	Go *rawGoLimits `json:"go"`
}

// rawLimitsAnswer is the usage command's answer: the limits sit under data
// when the runtime wraps them, and data is null before a console sign-in.
type rawLimitsAnswer struct {
	rawLimitsBody
	Data *rawLimitsBody `json:"data"`
}

// limitsArgs returns the usage limits invocation's argument vector.
func limitsArgs() []string {
	return []string{"api", "experimental.usage.limits"}
}

// pullLimits asks the runtime for the console limits of its subscription.
// Every failure to get an answer returns [usage.ErrPullUnsupported]: a
// build without the command, a missing sign-in, or a command outliving ctx
// does not start answering within the session.
func pullLimits(ctx context.Context, state *sessionState) ([]usage.WindowReading, error) {
	cmd, err := auxiliaryCommand(ctx, state, limitsArgs())
	if err != nil {
		return nil, fmt.Errorf("build opencode usage limits command: %w: %w", usage.ErrPullUnsupported, err)
	}

	var stdout bytes.Buffer
	result, startErr := procutil.RunCapture(ctx, cmd, procutil.StopGrace(state.agentConfig.StopGraceMS), procutil.CaptureParams{
		Stdout: &stdout,
		Logger: state.logger(),
	})
	if startErr != nil {
		return nil, fmt.Errorf("run opencode usage limits: %w: %w", usage.ErrPullUnsupported, startErr)
	}
	if result.WaitErr != nil {
		return nil, fmt.Errorf("run opencode usage limits: %w: %w", usage.ErrPullUnsupported, result.WaitErr)
	}

	var answer rawLimitsAnswer
	if err := json.NewDecoder(&stdout).Decode(&answer); err != nil {
		return nil, fmt.Errorf("opencode usage limits printed no object: %w", usage.ErrPullUnsupported)
	}
	body := answer.rawLimitsBody
	if answer.Data != nil {
		body = *answer.Data
	}
	if body.Go == nil {
		return nil, fmt.Errorf("opencode usage limits reported no subscription: %w", usage.ErrPullUnsupported)
	}

	var readings []usage.WindowReading
	for _, member := range []struct {
		name  string
		meter *rawMeter
	}{
		{"rolling", body.Go.Rolling},
		{"weekly", body.Go.Weekly},
		{"monthly", body.Go.Monthly},
	} {
		if member.meter == nil {
			continue
		}
		reading := usage.WindowReading{
			Window:   member.name,
			Share:    member.meter.Percent,
			ResetsAt: member.meter.ResetsAt / 1000,
		}
		if member.meter.Limited {
			reading.Status = limitedStatus
		}
		readings = append(readings, reading)
	}
	return readings, nil
}

// freeLimitReadings returns the one reading a free usage limit error
// carries, or nil for any other error. The retry delay is relative to the
// failure and in milliseconds.
func freeLimitReadings(runErr *rawRunError) []usage.WindowReading {
	if runErr == nil || runErr.Type != freeLimitType {
		return nil
	}
	reading := usage.WindowReading{Window: freeLimitType, Status: freeLimitType}
	if runErr.RetryAfterMs != nil {
		reading.ResetsAfter = time.Duration(*runErr.RetryAfterMs) * time.Millisecond
	}
	return []usage.WindowReading{reading}
}

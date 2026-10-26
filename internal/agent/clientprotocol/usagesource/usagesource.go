// Package usagesource holds the per-runtime parts of this transport that the
// protocol itself cannot express: the measurement sources it reads outside the
// wire, for runtimes whose protocol carries no spend counter, and the
// credential-verification additions that drop a runtime's agent context.
//
// Every source here is vendor-specific by construction; the transport names
// none of them and asks each the same three questions in order: does it claim
// this launch, does the handshake report the runtime's name, and what has it
// read for this session so far. A no to either of the first two leaves the
// session unmeasured. An addition is asked one question, whether it claims the
// launch.
package usagesource

import (
	"context"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/registry"
)

// Constructor returns a fresh Reader for one session.
type Constructor func() Reader

// Sources is the registry every source adds itself to, exactly once, from an
// init function in its own file. The key is the name of the runtime the
// source reads, a name its Recognize accepts.
var Sources = registry.NewRegistry[Constructor, struct{}]("usage source")

// Reader is one measurement source. The transport declares its own interface
// with this method set and holds values of it.
//
// A Reader is used from two goroutines and only in this order: Claim and Close
// on the session's own goroutine, and Recognize, Open and Drain on the message
// pump. Drain is the one blocking method and runs on a goroutine the pump
// starts, not the pump itself.
type Reader interface {
	// Claim reports whether this source can measure a session launched against
	// target and, when it can, arms itself and returns the environment
	// assignments ("NAME=value") the launch needs. It refuses every launch with
	// target.RemoteCommand set. With runtime empty (the first offer), a source
	// whose claim returns an assignment or creates a file or directory claims
	// only a local launch whose command line (target.Command and every element
	// of target.Args) names the runtime it reads; a claim that returns no
	// assignment and creates nothing may be made for any local launch. With
	// runtime non-empty (the relaunch offer, carrying the name the first
	// start's handshake reported), it claims a local launch exactly when
	// Recognize(runtime) is true. A refusal returns nil, false and creates
	// nothing.
	Claim(target agentcore.LaunchTarget, runtime string) ([]string, bool)

	// Recognize reports whether name, the runtime's own name from the
	// handshake, is the runtime this source maps. No build is refused by its
	// version.
	Recognize(name string) bool

	// Open records the session identifier whose records this source counts and
	// the position in its own output the session starts at, so a resumed
	// runtime's history contributes nothing.
	Open(sessionID string)

	// Drain returns the session's run-cumulative usage as this source measured
	// it, waiting up to its own deadline for the turn's records to become
	// readable. lowerBound is the turn's own input-plus-output figure from the
	// protocol's presence signal, letting the wait end early; a non-positive
	// lowerBound means no such signal was read. found is false when the source
	// measured nothing at all, which is not a measurement of zero.
	Drain(ctx context.Context, lowerBound int64) (usage agentcore.RecoveredUsage, source string, found bool)

	// Completeness reports how far the figure the most recent Drain returned was
	// proven to account for that turn; a figure can be a measurement and still
	// be short of the turn. It describes one Drain call and is reset by the
	// next, so it is read on the goroutine that called Drain, before another
	// begins.
	Completeness() Completeness

	// Close releases whatever Claim armed. It is safe to call on a source that
	// never claimed.
	Close()
}

// Completeness is how far a drained figure was proven to account for the turn
// it was drained for. It is not a verdict on whether a measurement exists.
type Completeness int

const (
	// CompletenessUnknown is a turn with no bound to prove a figure against,
	// which differs from proving it short.
	CompletenessUnknown Completeness = iota

	// CompletenessPartial is a figure that did not reach the turn's own bound.
	CompletenessPartial

	// CompletenessAccounted is a figure that reached the turn's own bound.
	CompletenessAccounted
)

// VerificationAddition is one protocol runtime's verification-context
// switches. The transport holds values of it and names no runtime.
//
// An addition is used from the session's own goroutine only: Claim at start,
// Close at teardown.
type VerificationAddition interface {
	// Claim reports whether the addition applies to target and, when it does,
	// creates the file its switches name and returns them. It refuses a remote
	// launch, whose host and command line are not Sortie's to extend, and a
	// command line that does not name its runtime; a refusal creates nothing.
	Claim(target agentcore.LaunchTarget) (VerificationSwitches, bool)

	// Context reports which agent context components the verification request
	// still carries when the switches apply.
	Context() registry.VerificationContext

	// Close removes what Claim created. It is safe to call on an addition that
	// never claimed and more than once.
	Close()
}

// VerificationSwitches are the launch changes one addition makes.
type VerificationSwitches struct {
	// Env holds "NAME=value" entries appended to the launch environment.
	Env []string

	// Args are appended after the operator's command line.
	Args []string
}

// VerificationAdditions is the registry every addition adds itself to,
// exactly once, from an init function in its own file. The key is the name of
// the runtime the addition serves.
var VerificationAdditions = registry.NewRegistry[func() VerificationAddition, struct{}]("verification addition")

// executableSuffixes are the platform-specific extensions a launcher may
// append to an executable name; namesRuntime strips at most one.
var executableSuffixes = []string{".cmd", ".ps1", ".exe", ".js"}

// namesRuntime reports whether the tokens of target's command line name the
// executable or one of the npm packages. A remote launch names nothing: its
// command line is a string the remote shell parses, not tokens Sortie owns.
func namesRuntime(target agentcore.LaunchTarget, executable string, packages ...string) bool {
	if target.RemoteCommand != "" {
		return false
	}
	tokens := append([]string{target.Command}, target.Args...)
	for i, token := range tokens {
		var optionValue bool
		if i > 0 {
			prev := tokens[i-1]
			optionValue = strings.HasPrefix(prev, "-") && prev != "--"
		}
		if runtimeToken(token, optionValue, executable, packages) {
			return true
		}
	}
	return false
}

func runtimeToken(token string, optionValue bool, executable string, packages []string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(token, `\`, "/"))
	if executableToken(normalized, optionValue, executable) {
		return true
	}
	for _, pkg := range packages {
		if normalized == pkg || strings.HasPrefix(normalized, pkg+"@") {
			return true
		}
		if strings.Contains(normalized, "/"+pkg+"/") || strings.HasSuffix(normalized, "/"+pkg) {
			return true
		}
	}
	return false
}

// executableToken reports whether a normalized token names the executable
// itself, bare or as a path. A bare option value with no path separator is
// ambiguous with an unrelated flag argument and is not matched.
func executableToken(normalized string, optionValue bool, executable string) bool {
	hasPathSeparator := strings.Contains(normalized, "/")
	if !hasPathSeparator && optionValue {
		return false
	}

	base := normalized
	if idx := strings.LastIndex(normalized, "/"); idx >= 0 {
		base = normalized[idx+1:]
	}
	for _, suffix := range executableSuffixes {
		if trimmed, ok := strings.CutSuffix(base, suffix); ok {
			base = trimmed
			break
		}
	}
	return base == executable
}

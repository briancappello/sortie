// Package environmenttest provides test doubles for the execution
// environment interface. It registers no kind.
package environmenttest

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"sync"

	"github.com/sortie-ai/sortie/internal/environment"
)

// Operation names one [environment.Environment] method a [Recorder]
// records.
type Operation string

// The recorded operations.
const (
	OpPrepare     Operation = "Prepare"
	OpCommand     Operation = "Command"
	OpRelease     Operation = "Release"
	OpRemove      Operation = "Remove"
	OpSweep       Operation = "Sweep"
	OpAfterCreate Operation = "AfterCreate"
)

// Call is one recorded operation.
type Call struct {
	Op        Operation
	Workspace environment.Workspace

	// Spec is the command specification of an [OpCommand] call.
	Spec environment.CommandSpec
}

// Recorder is an [environment.Environment] and
// [environment.AfterCreateFinisher] that records every call in order and
// delegates each command to Inner, so a test can assert which workspace
// code went through the environment while that code still runs. The
// zero value records calls and fails every command. Safe for concurrent
// use.
type Recorder struct {
	// KindName is returned by Kind; empty returns "recorder".
	KindName string

	// Inner builds the commands. Nil fails every command.
	Inner environment.Environment

	// GuestPathFunc maps a host path to the guest path; nil is the
	// identity.
	GuestPathFunc func(hostPath string) string

	// PrepareResult is what Prepare reports.
	PrepareResult environment.PrepareResult

	// PrepareErr, AfterCreateErr, and RemoveErr fail the operation.
	PrepareErr     error
	AfterCreateErr error
	RemoveErr      error

	mu    sync.Mutex
	calls []Call
}

var (
	_ environment.Environment         = (*Recorder)(nil)
	_ environment.AfterCreateFinisher = (*Recorder)(nil)
)

func (r *Recorder) record(c Call) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

// Calls returns a copy of every recorded call, in order.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// Ops returns the recorded operation names, in order.
func (r *Recorder) Ops() []Operation {
	calls := r.Calls()
	ops := make([]Operation, 0, len(calls))
	for _, c := range calls {
		ops = append(ops, c.Op)
	}
	return ops
}

// Commands returns the recorded [OpCommand] calls, in order.
func (r *Recorder) Commands() []Call {
	var cmds []Call
	for _, c := range r.Calls() {
		if c.Op == OpCommand {
			cmds = append(cmds, c)
		}
	}
	return cmds
}

// Kind returns KindName, or "recorder" when it is empty.
func (r *Recorder) Kind() string {
	if r.KindName == "" {
		return "recorder"
	}
	return r.KindName
}

// GuestPath applies GuestPathFunc, or returns hostPath unchanged.
func (r *Recorder) GuestPath(hostPath string) string {
	if r.GuestPathFunc == nil {
		return hostPath
	}
	return r.GuestPathFunc(hostPath)
}

// Prepare records the call and returns PrepareResult and PrepareErr.
func (r *Recorder) Prepare(_ context.Context, ws environment.Workspace) (environment.PrepareResult, error) {
	r.record(Call{Op: OpPrepare, Workspace: ws})
	return r.PrepareResult, r.PrepareErr
}

// Command records the call and returns Inner's command for spec.
func (r *Recorder) Command(ctx context.Context, ws environment.Workspace, spec environment.CommandSpec) (*exec.Cmd, error) {
	spec.Argv = slices.Clone(spec.Argv)
	spec.Env = slices.Clone(spec.Env)
	r.record(Call{Op: OpCommand, Workspace: ws, Spec: spec})
	if r.Inner == nil {
		return nil, errors.New("environmenttest: recorder has no inner environment")
	}
	return r.Inner.Command(ctx, ws, spec)
}

// Release records the call.
func (r *Recorder) Release(_ context.Context, ws environment.Workspace) error {
	r.record(Call{Op: OpRelease, Workspace: ws})
	return nil
}

// Remove records the call and returns RemoveErr.
func (r *Recorder) Remove(_ context.Context, ws environment.Workspace) error {
	r.record(Call{Op: OpRemove, Workspace: ws})
	return r.RemoveErr
}

// Sweep records the call.
func (r *Recorder) Sweep(context.Context, environment.SweepParams) error {
	r.record(Call{Op: OpSweep})
	return nil
}

// AfterCreate records the call and returns AfterCreateErr.
func (r *Recorder) AfterCreate(_ context.Context, ws environment.Workspace) error {
	r.record(Call{Op: OpAfterCreate, Workspace: ws})
	return r.AfterCreateErr
}

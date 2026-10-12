//go:build unix

package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/environment/environmenttest"
)

// guestEmulator runs each command on the host through the local kind,
// with every path under /run/sortie rewritten into a host directory that
// stands in for the guest's tmpfs.
type guestEmulator struct {
	environment.Environment
	guestRoot string
}

func (g guestEmulator) Command(ctx context.Context, ws environment.Workspace, spec environment.CommandSpec) (*exec.Cmd, error) {
	rewrite := func(s string) string { return strings.ReplaceAll(s, "/run/sortie/", g.guestRoot+"/run/sortie/") }
	spec.Argv = slices.Clone(spec.Argv)
	for i := range spec.Argv {
		spec.Argv[i] = rewrite(spec.Argv[i])
	}
	spec.Env = slices.Clone(spec.Env)
	for i := range spec.Env {
		spec.Env[i] = rewrite(spec.Env[i])
	}
	return g.Environment.Command(ctx, ws, spec)
}

// guestTriageEnvironment is a recording environment that runs
// elsewhere: a carrier, resumable, whose workspace is /workspace inside,
// and whose commands see a guest filesystem of their own.
type guestTriageEnvironment struct {
	*environmenttest.ResumingRecorder
	guestRoot string
}

var (
	_ environment.Carrier = guestTriageEnvironment{}
	_ environment.Resumer = guestTriageEnvironment{}
)

func (guestTriageEnvironment) LaunchArgv(_ environment.Workspace, launch domain.CarrierLaunch) ([]string, error) {
	return launch.Argv, nil
}

func (guestTriageEnvironment) ConnectionFailed(environment.Workspace, int, []string) bool {
	return false
}

func newGuestTriageEnvironment(t *testing.T) guestTriageEnvironment {
	t.Helper()
	local, err := environment.New(environment.Params{Config: config.EnvironmentConfig{Kind: config.EnvironmentKindLocal}})
	if err != nil {
		t.Fatalf("environment.New(local) error = %v", err)
	}
	guestRoot := t.TempDir()
	rec := &environmenttest.Recorder{
		KindName:      "guest",
		Inner:         guestEmulator{Environment: local, guestRoot: guestRoot},
		GuestPathFunc: func(string) string { return "/workspace" },
	}
	return guestTriageEnvironment{
		ResumingRecorder: &environmenttest.ResumingRecorder{Recorder: rec, Ready: true},
		guestRoot:        guestRoot,
	}
}

var triageGuestDirPattern = regexp.MustCompile(`^/run/sortie/triage/ci-[0-9a-f]{16}$`)

func runGuestTriage(t *testing.T, env guestTriageEnvironment, identifier, script string) (ReactionTriageOutcome, string) {
	t.Helper()
	root := mustTriageWorkspace(t, identifier)
	req := gateRequest(root, identifier, "fp-guest")
	req.Environment = env
	outcome := RunReactionTriage(context.Background(), config.ReactionTriageConfig{Script: script, TimeoutMS: 5000}, req, discardLogger())
	return outcome, filepath.Join(root, identifier)
}

func specEnv(spec environment.CommandSpec, name string) string {
	for _, entry := range spec.Env {
		if v, ok := strings.CutPrefix(entry, name+"="); ok {
			return v
		}
	}
	return ""
}

func TestRunReactionTriage_GuestFilesLiveInTheEnvironment(t *testing.T) {
	t.Parallel()

	env := newGuestTriageEnvironment(t)
	capture := filepath.Join(t.TempDir(), "input-seen.json")
	script := `cat "$SORTIE_REACTION_INPUT" > '` + capture + `' && echo '{"disposition":"handled"}' > "$SORTIE_REACTION_RESULT"`

	outcome, wsPath := runGuestTriage(t, env, "TRIAGE-GUEST", script)

	if outcome.Disposition != TriageHandled || outcome.Fallback != "" {
		t.Fatalf("outcome = %+v, want handled without a fallback", outcome)
	}
	ops := env.Ops()
	want := []environmenttest.Operation{environmenttest.OpResume, environmenttest.OpCommand, environmenttest.OpCommand, environmenttest.OpCommand, environmenttest.OpCommand}
	if !slices.Equal(ops, want) {
		t.Fatalf("environment ops = %q, want %q: resume, write, triage, read, remove", ops, want)
	}
	cmds := env.Commands()
	write, hook, read, remove := cmds[0].Spec, cmds[1].Spec, cmds[2].Spec, cmds[3].Spec

	if len(write.Argv) != 6 || write.Argv[0] != "sh" || !strings.Contains(write.Argv[2], "mkdir -p") || !strings.Contains(write.Argv[2], `cat > "$2"`) || !write.Stdin {
		t.Fatalf("write command = %+v, want sh -c mkdir and cat with stdin open", write)
	}
	dir := write.Argv[4]
	if !triageGuestDirPattern.MatchString(dir) || write.Argv[5] != dir+"/input.json" {
		t.Errorf("write argv = %q, want the run directory /run/sortie/triage/<run id> and its input.json", write.Argv)
	}
	if got := specEnv(hook, "SORTIE_REACTION_INPUT"); got != dir+"/input.json" {
		t.Errorf("SORTIE_REACTION_INPUT = %q, want %q", got, dir+"/input.json")
	}
	if got := specEnv(hook, "SORTIE_REACTION_RESULT"); got != dir+"/result.json" {
		t.Errorf("SORTIE_REACTION_RESULT = %q, want %q", got, dir+"/result.json")
	}
	if !strings.Contains(read.Argv[2], "head -c 65537") || read.Argv[len(read.Argv)-1] != dir+"/result.json" {
		t.Errorf("read command = %q, want head -c of the result file, one byte past the cap", read.Argv)
	}
	if !strings.Contains(remove.Argv[2], "rm -rf") || remove.Argv[len(remove.Argv)-1] != dir {
		t.Errorf("remove command = %q, want the run directory removed", remove.Argv)
	}

	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("the command did not see its input: %v", err)
	}
	var doc triageInputDocument
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Workspace != "/workspace" || doc.Fingerprint != "fp-guest" || doc.ReactionKind != "ci" {
		t.Errorf("input document = %s (%v), want the guest workspace, fingerprint, and kind", raw, err)
	}

	if entries, err := os.ReadDir(wsPath); err != nil || len(entries) != 0 {
		t.Errorf("workspace entries = %v, %v, want nothing written into the workspace tree", entries, err)
	}
	if entries, err := os.ReadDir(filepath.Join(env.guestRoot, "run", "sortie", "triage")); err != nil || len(entries) != 0 {
		t.Errorf("guest triage directory entries = %v, %v, want the run directory removed", entries, err)
	}
}

func TestRunReactionTriage_GuestResultFallbacks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		script string
		want   string
	}{
		{name: "no result", script: "true", want: triageFallbackNoResult},
		{name: "malformed", script: `printf 'not json' > "$SORTIE_REACTION_RESULT"`, want: triageFallbackMalformedResult},
		{name: "unknown disposition", script: `echo '{"disposition":"maybe"}' > "$SORTIE_REACTION_RESULT"`, want: triageFallbackUnknownDisposition},
		{name: "at the cap", script: `printf '{"disposition":"handled"}' > "$SORTIE_REACTION_RESULT" && head -c 65511 /dev/zero | tr '\0' ' ' >> "$SORTIE_REACTION_RESULT"`, want: ""},
		{name: "past the cap", script: `head -c 65537 /dev/zero | tr '\0' ' ' > "$SORTIE_REACTION_RESULT"`, want: triageFallbackResultTooLarge},
		{name: "directory in place of the result", script: `mkdir "$SORTIE_REACTION_RESULT"`, want: triageFallbackMalformedResult},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newGuestTriageEnvironment(t)
			outcome, _ := runGuestTriage(t, env, "TRIAGE-GUEST-FB", tt.script)
			if outcome.Fallback != tt.want {
				t.Errorf("outcome = %+v, want fallback %q", outcome, tt.want)
			}
			if tt.want == "" && outcome.Disposition != TriageHandled {
				t.Errorf("outcome = %+v, want handled", outcome)
			}
		})
	}
}

func TestRunReactionTriage_GuestNotRunnable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(env guestTriageEnvironment)
		ops   []environmenttest.Operation
	}{
		{name: "no instance", setup: func(env guestTriageEnvironment) { env.Ready = false },
			ops: []environmenttest.Operation{environmenttest.OpResume}},
		{name: "resume fails", setup: func(env guestTriageEnvironment) { env.ResumeErr = errors.New("smolvm hung") },
			ops: []environmenttest.Operation{environmenttest.OpResume}},
		{name: "input write fails", setup: func(env guestTriageEnvironment) {
			// A file where the guest's /run is makes mkdir fail.
			if err := os.WriteFile(filepath.Join(env.guestRoot, "run"), nil, 0o600); err != nil {
				panic(err)
			}
		}, ops: []environmenttest.Operation{environmenttest.OpResume, environmenttest.OpCommand, environmenttest.OpCommand}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newGuestTriageEnvironment(t)
			tt.setup(env)
			marker := filepath.Join(t.TempDir(), "ran")
			outcome, _ := runGuestTriage(t, env, "TRIAGE-GUEST-NR", "touch '"+marker+"'")
			if outcome.Fallback != triageFallbackStartFailed {
				t.Errorf("outcome = %+v, want fallback %q", outcome, triageFallbackStartFailed)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Errorf("triage command ran (stat error %v), want it never started", err)
			}
			// The failed write is followed only by the run directory's removal.
			if got := env.Ops(); !slices.Equal(got, tt.ops) {
				t.Errorf("environment ops = %q, want %q", got, tt.ops)
			}
		})
	}
}

func TestRunReactionTriage_EnvironmentOnTheHostKeepsHostFiles(t *testing.T) {
	t.Parallel()

	// A recorder that is no carrier runs commands on this host, as the
	// local kind does.
	env := &environmenttest.ResumingRecorder{Recorder: recordingEnvironment(t), Ready: true}
	root := mustTriageWorkspace(t, "TRIAGE-HOSTENV")
	req := gateRequest(root, "TRIAGE-HOSTENV", "fp-hostenv")
	req.Environment = env
	outcome := RunReactionTriage(context.Background(), config.ReactionTriageConfig{Script: triageVerdictScript("handled"), TimeoutMS: 5000}, req, discardLogger())

	if outcome.Disposition != TriageHandled {
		t.Errorf("outcome = %+v, want handled", outcome)
	}
	if got := env.Ops(); !slices.Equal(got, []environmenttest.Operation{environmenttest.OpCommand}) {
		t.Errorf("environment ops = %q, want only the triage command: no resume, no guest file commands", got)
	}
}

func TestSanitizeTriagePathSegment(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"ci":              "ci",
		"bot-review":      "bot-review",
		"merge_conflict":  "merge_conflict",
		"../x y":          "___x_y",
		"a/b\\c":          "a_b_c",
		"":                "run",
		"k\u00e9y\n":      "k_y_",
		"$(touch pwned)'": "__touch_pwned__",
	} {
		if got := sanitizeTriagePathSegment(in); got != want {
			t.Errorf("sanitizeTriagePathSegment(%q) = %q, want %q", in, got, want)
		}
	}
	if id := triageRunID("../ci"); !regexp.MustCompile(`^___ci-[0-9a-f]{16}$`).MatchString(id) {
		t.Errorf("triageRunID(../ci) = %q, want the sanitized kind and a random hex suffix", id)
	}
	if first, second := triageRunID("ci"), triageRunID("ci"); first == second {
		t.Error("triageRunID() repeated an identifier")
	}
}

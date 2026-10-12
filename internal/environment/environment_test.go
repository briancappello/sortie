package environment_test

import (
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/environment/environmenttest"
	_ "github.com/sortie-ai/sortie/internal/environment/local"
)

func TestNew(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"", config.EnvironmentKindLocal} {
		env, err := environment.New(environment.Params{Config: config.EnvironmentConfig{Kind: kind}})
		if err != nil {
			t.Fatalf("New(kind %q) error = %v", kind, err)
		}
		if env.Kind() != config.EnvironmentKindLocal {
			t.Errorf("New(kind %q).Kind() = %q, want %q", kind, env.Kind(), config.EnvironmentKindLocal)
		}
	}
}

func TestNew_UnregisteredKindFails(t *testing.T) {
	t.Parallel()

	env, err := environment.New(environment.Params{Config: config.EnvironmentConfig{Kind: "no-such-kind"}})
	if err == nil {
		t.Fatalf("New() = %v, want an error for an unregistered kind", env)
	}
}

var errFailingConstructor = errors.New("binary missing")

func TestNew_ConstructorErrorNamesKind(t *testing.T) {
	t.Parallel()

	const kind = "environment-test-failing"
	if !environment.Registry.Has(kind) {
		environment.Registry.Register(kind, func(environment.Params) (environment.Environment, error) {
			return nil, errFailingConstructor
		})
	}

	_, err := environment.New(environment.Params{Config: config.EnvironmentConfig{Kind: kind}})
	if !errors.Is(err, errFailingConstructor) {
		t.Fatalf("New() error = %v, want it to wrap %v", err, errFailingConstructor)
	}
}

func TestNew_PassesParams(t *testing.T) {
	t.Parallel()

	const kind = "environment-test-params"
	if !environment.Registry.Has(kind) {
		environment.Registry.Register(kind, func(p environment.Params) (environment.Environment, error) {
			return &paramsEnvironment{Recorder: environmenttest.Recorder{KindName: kind}, params: p}, nil
		})
	}

	want := environment.Params{Config: config.EnvironmentConfig{Kind: kind}, StateDir: "/state", WorkflowPath: "/wf/WORKFLOW.md"}
	env, err := environment.New(want)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	got := env.(*paramsEnvironment).params
	if env.Kind() != kind || got.StateDir != want.StateDir || got.WorkflowPath != want.WorkflowPath || got.Config.Kind != kind {
		t.Errorf("constructor params = %+v, kind %q; want %+v", got, env.Kind(), want)
	}
}

var errHostCheck = errors.New("host cannot run this kind")

func TestCheckHost(t *testing.T) {
	t.Parallel()

	const kind = "environment-test-host-check"
	if !environment.Registry.Has(kind) {
		environment.Registry.RegisterWithMeta(kind, func(environment.Params) (environment.Environment, error) {
			return &environmenttest.Recorder{KindName: kind}, nil
		}, environment.Meta{CheckHost: func(cfg config.EnvironmentConfig) error {
			if cfg.Kind != kind {
				return errors.New("check received another kind's configuration")
			}
			return errHostCheck
		}})
	}

	if err := environment.CheckHost(config.EnvironmentConfig{Kind: kind}); !errors.Is(err, errHostCheck) {
		t.Errorf("CheckHost(%q) = %v, want the kind's own check error", kind, err)
	}
	for _, k := range []string{"", config.EnvironmentKindLocal} {
		if err := environment.CheckHost(config.EnvironmentConfig{Kind: k}); err != nil {
			t.Errorf("CheckHost(kind %q) = %v, want nil: local declares no check", k, err)
		}
	}
	if err := environment.CheckHost(config.EnvironmentConfig{Kind: "no-such-kind"}); err == nil {
		t.Error("CheckHost(unregistered kind) = nil, want an error")
	}
}

// paramsEnvironment keeps the params its constructor received.
type paramsEnvironment struct {
	environmenttest.Recorder
	params environment.Params
}

package config

import (
	"errors"
	"reflect"
	"testing"
)

func TestEnvironmentConfig_Remote(t *testing.T) {
	t.Parallel()

	cfg, err := NewServiceConfig(map[string]any{
		"environment": map[string]any{
			"kind": "remote",
			"remote": map[string]any{
				"requires": []any{"linux", "amd64"}, "cpus": 2, "memory_mib": 2048, "disk_mib": 4096,
				"flows": []any{"git"}, "lease_ttl_ms": 30000,
			},
		},
		"worker": map[string]any{"identity_header": "X-Forwarded-User"},
	})
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v", err)
	}
	want := RemoteConfig{IdentityHeader: "X-Forwarded-User", Requires: []string{"linux", "amd64"},
		CPUs: 2, MemoryMiB: 2048, DiskMiB: 4096, Flows: []string{"git"}, LeaseTTLMS: 30000}
	if cfg.Environment.Kind != EnvironmentKindRemote || !reflect.DeepEqual(cfg.Environment.Remote, want) {
		t.Fatalf("Environment = %+v, want remote %+v", cfg.Environment, want)
	}
}

func TestEnvironmentConfig_RemoteDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := NewServiceConfig(map[string]any{"environment": map[string]any{"kind": "remote"}})
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v", err)
	}
	want := RemoteConfig{IdentityHeader: DefaultWorkerIdentityHeader, LeaseTTLMS: 60000}
	if !reflect.DeepEqual(cfg.Environment.Remote, want) {
		t.Fatalf("Remote = %+v, want %+v", cfg.Environment.Remote, want)
	}
}

func TestEnvironmentConfig_RemoteRejects(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		raw   map[string]any
		field string
	}{
		"ssh hosts": {map[string]any{
			"environment": map[string]any{"kind": "remote"},
			"worker":      map[string]any{"ssh_hosts": []any{"h"}},
		}, "environment.kind"},
		"bad header": {map[string]any{
			"environment": map[string]any{"kind": "remote"},
			"worker":      map[string]any{"identity_header": "X: y"},
		}, "worker.identity_header"},
		"negative cpus": {map[string]any{
			"environment": map[string]any{"kind": "remote", "remote": map[string]any{"cpus": -1}},
		}, "environment.remote.cpus"},
		"zero lease": {map[string]any{
			"environment": map[string]any{"kind": "remote", "remote": map[string]any{"lease_ttl_ms": 0}},
		}, "environment.remote.lease_ttl_ms"},
		"requires not list": {map[string]any{
			"environment": map[string]any{"kind": "remote", "remote": map[string]any{"requires": "linux"}},
		}, "environment.remote.requires"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := NewServiceConfig(tc.raw)
			var ce *ConfigError
			if !errors.As(err, &ce) || ce.Field != tc.field {
				t.Fatalf("error = %v, want a ConfigError on %s", err, tc.field)
			}
		})
	}
}

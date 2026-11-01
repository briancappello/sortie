package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEnvironmentConfig_SmolvmHostInputsDefaultOff(t *testing.T) {
	t.Parallel()

	cfg, err := NewServiceConfig(smolvmRaw(writePack(t), map[string]any{"allow_hosts": []any{"ghcr.io"}}))
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v, want nil", err)
	}
	s := cfg.Environment.Smolvm
	if s.Nested || s.NetBackend != "" || s.Mounts != nil {
		t.Errorf("Smolvm nested = %v, net_backend = %q, mounts = %+v; want all off when the workflow sets none", s.Nested, s.NetBackend, s.Mounts)
	}

	cfg, err = NewServiceConfig(smolvmRaw(writePack(t), map[string]any{"nested": false, "mounts": []any{}}))
	if err != nil {
		t.Fatalf("NewServiceConfig(nested: false, mounts: []) error = %v, want nil", err)
	}
	if s := cfg.Environment.Smolvm; s.Nested || s.Mounts != nil {
		t.Errorf("Smolvm nested = %v, mounts = %+v; want off", s.Nested, s.Mounts)
	}
}

func TestEnvironmentConfig_SmolvmHostInputs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sources := filepath.Join(home, ".cache", "homelab-sources")
	if err := os.MkdirAll(sources, 0o750); err != nil {
		t.Fatal(err)
	}
	caches := t.TempDir()
	t.Setenv("SORTIE_TEST_MOUNT_CACHES", caches)

	cfg, err := NewServiceConfig(smolvmRaw(writePack(t), map[string]any{
		"nested":      true,
		"net_backend": "virtio-net",
		"allow_hosts": []any{"ghcr.io"},
		"mounts": []any{
			map[string]any{"host": "~/.cache/homelab-sources", "guest": "/home/agent/.cache/homelab-sources", "read_only": true},
			map[string]any{"host": "$SORTIE_TEST_MOUNT_CACHES", "guest": "/home/agent/.cache/go/"},
			map[string]any{"host": "~/.local/state/sortie-recorder/{workspace}", "guest": "/var/recorder", "read_only": false},
		},
	}))
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v, want nil", err)
	}
	s := cfg.Environment.Smolvm
	if !s.Nested || s.NetBackend != SmolvmNetBackendVirtioNet {
		t.Errorf("Smolvm nested = %v, net_backend = %q; want true and virtio-net", s.Nested, s.NetBackend)
	}
	recorder := filepath.Join(home, ".local", "state", "sortie-recorder", "{workspace}")
	want := []SmolvmMount{
		{Host: sources, Guest: "/home/agent/.cache/homelab-sources", ReadOnly: true},
		{Host: caches, Guest: "/home/agent/.cache/go"},
		{Host: recorder, Guest: "/var/recorder"},
	}
	if !reflect.DeepEqual(s.Mounts, want) {
		t.Errorf("Smolvm.Mounts =\n %+v\nwant\n %+v", s.Mounts, want)
	}
	if s.Mounts[0].PerWorkspace() || !s.Mounts[2].PerWorkspace() {
		t.Errorf("PerWorkspace() = %v, %v; want false for a shared directory and true for a {workspace} one", s.Mounts[0].PerWorkspace(), s.Mounts[2].PerWorkspace())
	}
	if got, want := s.Mounts[2].HostFor("ONS-30"), filepath.Join(home, ".local", "state", "sortie-recorder", "ONS-30"); got != want {
		t.Errorf("HostFor(ONS-30) = %q, want %q", got, want)
	}
}

func TestEnvironmentConfig_SmolvmHostInputErrors(t *testing.T) {
	t.Parallel()

	pack := writePack(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	withMounts := func(entries ...any) map[string]any {
		return smolvmRaw(pack, map[string]any{"mounts": entries})
	}
	mount := func(host, guest string) map[string]any {
		return map[string]any{"host": host, "guest": guest}
	}

	tests := []struct {
		name      string
		raw       map[string]any
		wantField string
		wantMsg   []string
	}{
		{
			name:      "nested of the wrong type",
			raw:       smolvmRaw(pack, map[string]any{"nested": "yes"}),
			wantField: "environment.smolvm.nested",
			wantMsg:   []string{"expected boolean"},
		},
		{
			name:      "tsi cannot enforce the allowlist",
			raw:       smolvmRaw(pack, map[string]any{"net_backend": "tsi", "allow_hosts": []any{"ghcr.io"}}),
			wantField: "environment.smolvm.net_backend",
			wantMsg:   []string{`"tsi"`, "virtio-net", "unfiltered"},
		},
		{
			name:      "unknown backend",
			raw:       smolvmRaw(pack, map[string]any{"net_backend": "slirp", "allow_hosts": []any{"ghcr.io"}}),
			wantField: "environment.smolvm.net_backend",
			wantMsg:   []string{`"slirp"`, "virtio-net"},
		},
		{
			name:      "empty backend",
			raw:       smolvmRaw(pack, map[string]any{"net_backend": "", "allow_hosts": []any{"ghcr.io"}}),
			wantField: "environment.smolvm.net_backend",
			wantMsg:   []string{"empty"},
		},
		{
			name:      "backend of the wrong type",
			raw:       smolvmRaw(pack, map[string]any{"net_backend": 1, "allow_hosts": []any{"ghcr.io"}}),
			wantField: "environment.smolvm.net_backend",
			wantMsg:   []string{"expected string"},
		},
		{
			name:      "backend without a network",
			raw:       smolvmRaw(pack, map[string]any{"net_backend": "virtio-net"}),
			wantField: "environment.smolvm.net_backend",
			wantMsg:   []string{"allow_hosts or credentials"},
		},
		{
			name:      "mounts is not a list",
			raw:       smolvmRaw(pack, map[string]any{"mounts": "/data"}),
			wantField: "environment.smolvm.mounts",
			wantMsg:   []string{"expected list"},
		},
		{
			name:      "entry is not a mapping",
			raw:       withMounts("/data"),
			wantField: "environment.smolvm.mounts[0]",
			wantMsg:   []string{"expected mapping"},
		},
		{
			name:      "host missing",
			raw:       withMounts(map[string]any{"guest": "/data"}),
			wantField: "environment.smolvm.mounts[0].host",
			wantMsg:   []string{"required"},
		},
		{
			name:      "guest missing",
			raw:       withMounts(map[string]any{"host": dir}),
			wantField: "environment.smolvm.mounts[0].guest",
			wantMsg:   []string{"required"},
		},
		{
			name:      "relative host",
			raw:       withMounts(mount("cache", "/data")),
			wantField: "environment.smolvm.mounts[0].host",
			wantMsg:   []string{"absolute"},
		},
		{
			name:      "relative guest",
			raw:       withMounts(mount(dir, "data")),
			wantField: "environment.smolvm.mounts[0].guest",
			wantMsg:   []string{"absolute"},
		},
		{
			name:      "colon in host",
			raw:       withMounts(mount(dir+":ro", "/data")),
			wantField: "environment.smolvm.mounts[0].host",
			wantMsg:   []string{"':'"},
		},
		{
			name:      "colon in guest",
			raw:       withMounts(mount(dir, "/data:ro")),
			wantField: "environment.smolvm.mounts[0].guest",
			wantMsg:   []string{"':'"},
		},
		{
			name:      "host does not exist",
			raw:       withMounts(mount(filepath.Join(dir, "missing"), "/data")),
			wantField: "environment.smolvm.mounts[0].host",
			wantMsg:   []string{"does not exist"},
		},
		{
			name:      "host is a file",
			raw:       withMounts(mount(file, "/data")),
			wantField: "environment.smolvm.mounts[0].host",
			wantMsg:   []string{"not a directory"},
		},
		{
			name:      "two entries share a guest path",
			raw:       withMounts(mount(dir, "/data"), mount(dir, "/data/")),
			wantField: "environment.smolvm.mounts[1].guest",
			wantMsg:   []string{`"/data"`, "mounts[0]"},
		},
		{
			name:      "guest is the control directory",
			raw:       withMounts(mount(dir, "/data"), mount(dir, "/run/sortie/control")),
			wantField: "environment.smolvm.mounts[1].guest",
			wantMsg:   []string{"/run/sortie/control", "Sortie mounts itself"},
		},
		{
			name:      "guest lies under the control directory",
			raw:       withMounts(mount(dir, "/run/sortie/control/x")),
			wantField: "environment.smolvm.mounts[0].guest",
			wantMsg:   []string{"/run/sortie/control"},
		},
		{
			name:      "guest is the tool socket",
			raw:       withMounts(mount(dir, "/run/sortie/tools.sock")),
			wantField: "environment.smolvm.mounts[0].guest",
			wantMsg:   []string{"/run/sortie/tools.sock"},
		},
		{
			name:      "guest covers Sortie's own",
			raw:       withMounts(mount(dir, "/run/sortie")),
			wantField: "environment.smolvm.mounts[0].guest",
			wantMsg:   []string{"/run/sortie/control"},
		},
		{
			name:      "guest is the root",
			raw:       withMounts(mount(dir, "/")),
			wantField: "environment.smolvm.mounts[0].guest",
			wantMsg:   []string{"Sortie mounts itself"},
		},
		{
			name:      "read_only of the wrong type",
			raw:       withMounts(map[string]any{"host": dir, "guest": "/data", "read_only": "yes"}),
			wantField: "environment.smolvm.mounts[0].read_only",
			wantMsg:   []string{"expected boolean"},
		},
		{
			name: "per-workspace host under the workspace root",
			raw: func() map[string]any {
				raw := withMounts(mount(filepath.Join(root, "{workspace}", "rec"), "/var/recorder"))
				raw["workspace"] = map[string]any{"root": root}
				return raw
			}(),
			wantField: "environment.smolvm.mounts[0].host",
			wantMsg:   []string{"workspace.root"},
		},
		{
			name: "shared host is the workspace root",
			raw: func() map[string]any {
				raw := withMounts(mount(root, "/data"))
				raw["workspace"] = map[string]any{"root": root}
				return raw
			}(),
			wantField: "environment.smolvm.mounts[0].host",
			wantMsg:   []string{"workspace.root"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewServiceConfig(tt.raw)
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("NewServiceConfig() error = %v, want a *ConfigError", err)
			}
			if ce.Field != tt.wantField {
				t.Errorf("ConfigError.Field = %q, want %q (message %q)", ce.Field, tt.wantField, ce.Message)
			}
			for _, want := range tt.wantMsg {
				if !strings.Contains(ce.Message, want) {
					t.Errorf("ConfigError.Message = %q, want it to contain %q", ce.Message, want)
				}
			}
		})
	}
}

// The backend is accepted with credentials alone: their hosts join the
// allowlist, so smolvm filters the VM's egress.
func TestEnvironmentConfig_SmolvmNetBackendWithCredentialsOnly(t *testing.T) {
	t.Parallel()

	cfg, err := NewServiceConfig(smolvmRaw(writePack(t), map[string]any{
		"net_backend": "virtio-net",
		"credentials": []any{map[string]any{"name": "or", "env": "OPENROUTER_API_KEY", "hosts": []any{"openrouter.ai"}}},
	}))
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v, want nil", err)
	}
	if cfg.Environment.Smolvm.NetBackend != SmolvmNetBackendVirtioNet {
		t.Errorf("Smolvm.NetBackend = %q, want virtio-net", cfg.Environment.Smolvm.NetBackend)
	}
}

func TestValidateFrontMatter_SmolvmHostInputsAreKnown(t *testing.T) {
	t.Parallel()

	raw := smolvmRaw(writePack(t), map[string]any{
		"nested":      true,
		"net_backend": "virtio-net",
		"allow_hosts": []any{"ghcr.io"},
		"mounts":      []any{map[string]any{"host": t.TempDir(), "guest": "/data"}},
	})
	cfg, err := NewServiceConfig(raw)
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v, want nil", err)
	}
	if warnings := ValidateFrontMatter(raw, cfg); len(warnings) != 0 {
		t.Errorf("ValidateFrontMatter() = %+v, want no warning for nested, net_backend and mounts", warnings)
	}
}

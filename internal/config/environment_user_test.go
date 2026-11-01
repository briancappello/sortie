package config

import (
	"errors"
	"strings"
	"testing"
)

func TestEnvironmentConfig_SmolvmUser(t *testing.T) {
	t.Parallel()

	cfg, err := NewServiceConfig(smolvmRaw(writePack(t), nil))
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v, want nil", err)
	}
	if u := cfg.Environment.Smolvm.User; u != "" {
		t.Errorf("Smolvm.User = %q, want none when the workflow sets none", u)
	}

	for _, user := range []string{"builder", "agent.bot", "ci_user-2", "1000", "1000:1000", "0:0"} {
		cfg, err := NewServiceConfig(smolvmRaw(writePack(t), map[string]any{"user": user}))
		if err != nil {
			t.Errorf("NewServiceConfig(user: %q) error = %v, want nil", user, err)
			continue
		}
		if got := cfg.Environment.Smolvm.User; got != user {
			t.Errorf("Smolvm.User = %q, want %q", got, user)
		}
	}
}

func TestEnvironmentConfig_SmolvmUserErrors(t *testing.T) {
	t.Parallel()

	pack := writePack(t)
	tests := []struct {
		name    string
		raw     map[string]any
		field   string
		wantMsg []string
	}{
		{"empty", smolvmRaw(pack, map[string]any{"user": ""}), "environment.smolvm.user", []string{"empty"}},
		{"integer", smolvmRaw(pack, map[string]any{"user": 1000}), "environment.smolvm.user", []string{"string"}},
		{"space", smolvmRaw(pack, map[string]any{"user": "build er"}), "environment.smolvm.user", []string{`"build er"`, "uid[:gid]"}},
		{"slash", smolvmRaw(pack, map[string]any{"user": "a/b"}), "environment.smolvm.user", []string{`"a/b"`}},
		{"name and group", smolvmRaw(pack, map[string]any{"user": "builder:builder"}), "environment.smolvm.user", []string{"one ':' between two numbers"}},
		{"name and gid", smolvmRaw(pack, map[string]any{"user": "builder:1000"}), "environment.smolvm.user", []string{"uid[:gid]"}},
		{"missing gid", smolvmRaw(pack, map[string]any{"user": "1000:"}), "environment.smolvm.user", []string{"uid[:gid]"}},
		{"missing uid", smolvmRaw(pack, map[string]any{"user": ":1000"}), "environment.smolvm.user", []string{"uid[:gid]"}},
		{"two colons", smolvmRaw(pack, map[string]any{"user": "1:2:3"}), "environment.smolvm.user", []string{"uid[:gid]"}},
		{"non-ASCII", smolvmRaw(pack, map[string]any{"user": "bÿ"}), "environment.smolvm.user", []string{"uid[:gid]"}},
		{"shell", smolvmRaw(pack, map[string]any{"user": "x;id"}), "environment.smolvm.user", []string{"uid[:gid]"}},
		{
			"mount over the transfer directory",
			smolvmRaw(pack, map[string]any{"user": "builder", "mounts": []any{map[string]any{"host": t.TempDir(), "guest": "/tmp"}}}),
			"environment.smolvm.mounts[0].guest",
			[]string{SmolvmGuestUserTransferDir},
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
			if ce.Field != tt.field {
				t.Errorf("ConfigError.Field = %q, want %q (message %q)", ce.Field, tt.field, ce.Message)
			}
			for _, want := range tt.wantMsg {
				if !strings.Contains(ce.Message, want) {
					t.Errorf("ConfigError.Message = %q, want it to contain %q", ce.Message, want)
				}
			}
		})
	}
}

// Sortie stages bundles in /tmp/sortie-transfer only in a VM whose user
// is set, so without user a mount may cover it.
func TestEnvironmentConfig_SmolvmMountOnTmpWithoutUser(t *testing.T) {
	t.Parallel()

	_, err := NewServiceConfig(smolvmRaw(writePack(t), map[string]any{"mounts": []any{map[string]any{"host": t.TempDir(), "guest": "/tmp"}}}))
	if err != nil {
		t.Errorf("NewServiceConfig() error = %v, want nil", err)
	}
}

func TestValidateFrontMatter_SmolvmUserIsKnown(t *testing.T) {
	t.Parallel()

	raw := smolvmRaw(writePack(t), map[string]any{"user": "builder"})
	cfg, err := NewServiceConfig(raw)
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v, want nil", err)
	}
	if warnings := ValidateFrontMatter(raw, cfg); len(warnings) != 0 {
		t.Errorf("ValidateFrontMatter() = %+v, want no warning for user", warnings)
	}
}

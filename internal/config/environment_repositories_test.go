package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func repoEntry(name, url string) map[string]any {
	e := map[string]any{"url": url, "base": "main"}
	if name != "" {
		e["name"] = name
	}
	return e
}

// The workflow lists the repositories Sortie may open.
func TestEnvironmentConfig_Repositories(t *testing.T) {
	t.Parallel()

	pack := writePack(t)
	raw := smolvmRaw(pack, map[string]any{
		"repositories": []any{
			repoEntry("sortie", "https://example.com/o/sortie.git"),
			map[string]any{"name": "ao", "url": "https://example.com/o/ao.git", "base": "trunk", "provider": "github", "project": "o/ao"},
		},
		"default_repository": "ao",
	})
	cfg, err := NewServiceConfig(raw)
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v", err)
	}
	s := cfg.Environment.Smolvm
	if s.DefaultRepository != "ao" || s.Repository == nil || s.Repository.Name != "ao" || s.Repository.Base != "trunk" {
		t.Errorf("default = %q, Repository = %+v, want ao", s.DefaultRepository, s.Repository)
	}
	if got := s.RepositoryNames(); !reflect.DeepEqual(got, []string{"sortie", "ao"}) {
		t.Errorf("RepositoryNames() = %v", got)
	}
	r, ok := s.RepositoryNamed("sortie")
	if !ok || r.BranchPrefix != defaultRepoBranchPrefix || r.MaxBundleMiB != defaultRepoMaxBundleMiB {
		t.Errorf("RepositoryNamed(sortie) = %+v, %v, want defaults applied", r, ok)
	}
	if _, ok := s.RepositoryNamed("other"); ok {
		t.Error("RepositoryNamed(other) found a repository that is not listed")
	}
}

func TestEnvironmentConfig_RepositoriesOneEntryIsTheDefault(t *testing.T) {
	t.Parallel()

	cfg, err := NewServiceConfig(smolvmRaw(writePack(t), map[string]any{
		"repositories": []any{repoEntry("only", "https://example.com/o/only.git")},
	}))
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v", err)
	}
	if cfg.Environment.Smolvm.DefaultRepository != "only" || cfg.Environment.Smolvm.Repository.Name != "only" {
		t.Errorf("default = %q, want only", cfg.Environment.Smolvm.DefaultRepository)
	}
}

func TestEnvironmentConfig_SingleRepositoryUnchanged(t *testing.T) {
	t.Parallel()

	cfg, err := NewServiceConfig(smolvmRaw(writePack(t), map[string]any{
		"repository": repoEntry("", "https://example.com/o/app.git"),
	}))
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v", err)
	}
	s := cfg.Environment.Smolvm
	if s.Repository == nil || s.Repository.URL != "https://example.com/o/app.git" || s.DefaultRepository != "" {
		t.Fatalf("Repository = %+v, default %q", s.Repository, s.DefaultRepository)
	}
	if len(s.Repositories) != 1 || len(s.RepositoryNames()) != 0 {
		t.Errorf("Repositories = %+v, names %v; want the one unnamed block", s.Repositories, s.RepositoryNames())
	}
	if r, ok := s.RepositoryNamed(""); !ok || r.URL != s.Repository.URL {
		t.Errorf("RepositoryNamed(\"\") = %+v, %v", r, ok)
	}
}

func TestEnvironmentConfig_RepositoriesErrors(t *testing.T) {
	t.Parallel()

	a := repoEntry("a", "https://example.com/a.git")
	b := repoEntry("b", "https://example.com/b.git")
	tests := []struct {
		name  string
		extra map[string]any
		field string
		msg   string
	}{
		{"both forms", map[string]any{"repository": repoEntry("", "u"), "repositories": []any{a}}, smolvmRepositoriesField, "together"},
		{"default with single", map[string]any{"repository": repoEntry("", "u"), "default_repository": "a"}, smolvmDefaultRepoField, "single"},
		{"default without list", map[string]any{"default_repository": "a"}, smolvmDefaultRepoField, "lists none"},
		{"not a list", map[string]any{"repositories": "a"}, smolvmRepositoriesField, "sequence"},
		{"empty list", map[string]any{"repositories": []any{}}, smolvmRepositoriesField, "at least one"},
		{"entry not a map", map[string]any{"repositories": []any{"a"}}, smolvmRepositoriesField + "[0]", "mapping"},
		{"missing name", map[string]any{"repositories": []any{repoEntry("", "u")}}, smolvmRepositoriesField + "[0].name", "required"},
		{"bad name", map[string]any{"repositories": []any{repoEntry("Bad_Name", "u")}}, smolvmRepositoriesField + "[0].name", "must match"},
		{"duplicate name", map[string]any{"repositories": []any{a, repoEntry("a", "v")}}, smolvmRepositoriesField + "[1].name", "more than once"},
		{"missing url", map[string]any{"repositories": []any{map[string]any{"name": "a", "base": "main"}}}, smolvmRepositoriesField + "[0].url", "required"},
		{"two without default", map[string]any{"repositories": []any{a, b}}, smolvmDefaultRepoField, "required"},
		{"unlisted default", map[string]any{"repositories": []any{a, b}, "default_repository": "c"}, smolvmDefaultRepoField, "does not list"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewServiceConfig(smolvmRaw(writePack(t), tt.extra))
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("error = %v, want a *ConfigError", err)
			}
			if ce.Field != tt.field || !strings.Contains(ce.Message, tt.msg) {
				t.Errorf("error = %s: %s, want field %s containing %q", ce.Field, ce.Message, tt.field, tt.msg)
			}
		})
	}
}

func TestValidateFrontMatter_RepositoriesKnown(t *testing.T) {
	t.Parallel()

	raw := smolvmRaw("/p", map[string]any{
		"repositories":       []any{repoEntry("a", "u")},
		"default_repository": "a",
	})
	for _, w := range ValidateFrontMatter(raw, ServiceConfig{}) {
		if strings.Contains(w.Field, "repositor") {
			t.Errorf("unexpected warning %+v", w)
		}
	}
}

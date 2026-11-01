package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
)

func repositoriesConfig(providers ...string) config.ServiceConfig {
	cfg := repositoryConfig("")
	repos := make([]config.SmolvmRepository, 0, len(providers))
	for i, p := range providers {
		repos = append(repos, config.SmolvmRepository{
			Name: string(rune('a' + i)), URL: "https://example.com/org/" + string(rune('a'+i)) + ".git", Base: "main",
			BranchPrefix: "sortie/", Provider: p, Project: "org/x", MaxBundleMiB: 512,
		})
	}
	cfg.Environment.Smolvm = config.SmolvmConfig{Repositories: repos, DefaultRepository: "a", Repository: &repos[0]}
	return cfg
}

func TestCheckRepositoryProvider_Repositories(t *testing.T) {
	t.Parallel()

	if err := checkRepositoryProvider(repositoriesConfig("", "github", "github")); err != nil {
		t.Errorf("same providers: error = %v, want nil", err)
	}
	err := checkRepositoryProvider(repositoriesConfig("github", "gitea"))
	ce, ok := errors.AsType[*config.ConfigError](err)
	if !ok || ce.Field != "environment.smolvm.repositories[1].provider" || !strings.Contains(ce.Message, "environment.smolvm.repositories[0].provider") {
		t.Errorf("mixed providers: error = %v, want one naming both entries", err)
	}
	err = checkRepositoryProvider(repositoriesConfig("", "nosuchforge"))
	if ce, ok := errors.AsType[*config.ConfigError](err); !ok || ce.Field != "environment.smolvm.repositories[1].provider" {
		t.Errorf("unknown provider: error = %v", err)
	}
}

// A repositories list without a default still names each entry's
// provider by its index.
func TestCheckRepositoryProvider_RepositoriesWithoutDefault(t *testing.T) {
	t.Parallel()

	cfg := repositoriesConfig("github", "gitea")
	cfg.Environment.Smolvm.DefaultRepository, cfg.Environment.Smolvm.Repository = "", nil
	err := checkRepositoryProvider(cfg)
	if ce, ok := errors.AsType[*config.ConfigError](err); !ok || ce.Field != "environment.smolvm.repositories[1].provider" {
		t.Errorf("mixed providers: error = %v, want the second entry named", err)
	}
}

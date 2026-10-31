package smolvm

import (
	"net/http"
	"testing"

	"github.com/sortie-ai/sortie/internal/latestrelease"
	"github.com/sortie-ai/sortie/internal/waittest"
)

const smolvmRepo = "smol-machines/smolvm"

func latestSmolvm(t *testing.T) string {
	t.Helper()
	latest, err := latestrelease.GitHub(waittest.Context(t), http.DefaultClient, latestrelease.GitHubAPI, smolvmRepo)
	if err != nil {
		t.Fatalf("latest smolvm release: %v", err)
	}
	return latest
}

func TestLatest_SmolvmOnPath(t *testing.T) {
	latestrelease.Require(t)

	latest := latestSmolvm(t)
	ours, err := NewCLI(DefaultBinary).Version(waittest.Context(t))
	if err != nil {
		t.Fatalf("smolvm on PATH: %v", err)
	}
	if latestrelease.Compare(ours, latest) != 0 {
		t.Errorf("smolvm on PATH is not the latest release: ours %s, latest %s", ours, latest)
	}
}

func TestLatest_MinimumVersionIsReleased(t *testing.T) {
	latestrelease.Require(t)

	if latest := latestSmolvm(t); latestrelease.Compare(MinimumVersion, latest) > 0 {
		t.Errorf("smolvm minimum version %s is newer than the latest release %s", MinimumVersion, latest)
	}
}

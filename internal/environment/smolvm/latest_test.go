package smolvm

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/latestrelease"
)

const smolvmRepo = "smol-machines/smolvm"

func latestSmolvm(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	latest, err := latestrelease.GitHub(ctx, &http.Client{Timeout: time.Minute}, latestrelease.GitHubAPI, smolvmRepo)
	if err != nil {
		t.Fatalf("latest smolvm release: %v", err)
	}
	return latest
}

func TestLatest_SmolvmOnPath(t *testing.T) {
	latestrelease.Require(t)

	latest := latestSmolvm(t)
	ctx, cancel := context.WithTimeout(context.Background(), versionCheckTimeout)
	defer cancel()
	ours, err := NewCLI(DefaultBinary).Version(ctx)
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

package copilot

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

type quotaServer struct {
	hits atomic.Int32
	auth atomic.Value
}

func serveQuota(t *testing.T, status int, body []byte) *quotaServer {
	t.Helper()
	spy := &quotaServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spy.hits.Add(1)
		spy.auth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	previous := quotaEndpoint
	quotaEndpoint = srv.URL
	t.Cleanup(func() { quotaEndpoint = previous })
	return spy
}

func missingRuntimeTarget(t *testing.T) agentcore.LaunchTarget {
	t.Helper()
	return agentcore.LaunchTarget{Command: filepath.Join(t.TempDir(), "no-copilot"), WorkspacePath: t.TempDir()}
}

func TestPullQuota_TokenPrecedenceAndWindows(t *testing.T) {
	fixture, err := os.ReadFile("testdata/copilot_internal_user.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	tests := []struct {
		name string
		env  []string
		want string
	}{
		{"copilot token outranks the others", []string{"GITHUB_TOKEN=c", "GH_TOKEN=b", "COPILOT_GITHUB_TOKEN=a"}, "token a"},
		{"gh token outranks github token", []string{"GITHUB_TOKEN=c", "GH_TOKEN=b"}, "token b"},
		{"github token alone", []string{"GITHUB_TOKEN=c"}, "token c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := serveQuota(t, http.StatusOK, fixture)

			readings, err := pullQuota(t.Context(), missingRuntimeTarget(t), tt.env)
			if err != nil {
				t.Fatalf("pullQuota() error = %v, want nil", err)
			}
			windows := usage.Windows(quotaSource, readings, time.Now())

			if got, _ := spy.auth.Load().(string); got != tt.want {
				t.Errorf("Authorization = %q, want %q", got, tt.want)
			}
			if len(windows) != 1 {
				t.Fatalf("Windows() = %d windows, want 1 (the unlimited members have none)", len(windows))
			}
			w := windows[0]
			if w.Key != "premium_interactions" || w.Label != "Monthly premium request limit" ||
				w.UsedPercent == nil || *w.UsedPercent != 100 || !w.ResetsAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
				t.Errorf("Windows() = key %q label %q used %v resets %v, want premium_interactions, Monthly premium request limit, 100, 2026-11-01T00:00:00Z", w.Key, w.Label, w.UsedPercent, w.ResetsAt)
			}
		})
	}
}

func TestPullQuota_Failures(t *testing.T) {
	const token = "ghp_secret_value"

	tests := []struct {
		name      string
		env       []string
		status    int
		body      string
		wantHits  int32
		wantRoute bool
	}{
		{"unauthorized runs the headless route", []string{"GH_TOKEN=" + token}, http.StatusUnauthorized, "", 1, true},
		{"forbidden runs the headless route", []string{"GH_TOKEN=" + token}, http.StatusForbidden, "", 1, true},
		{"not found runs the headless route", []string{"GH_TOKEN=" + token}, http.StatusNotFound, "", 1, true},
		{"no token runs the headless route without a request", []string{}, http.StatusOK, "", 0, true},
		{"server error is an error", []string{"GH_TOKEN=" + token}, http.StatusInternalServerError, "", 1, false},
		{"malformed answer is an error", []string{"GH_TOKEN=" + token}, http.StatusOK, "{", 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := serveQuota(t, tt.status, []byte(tt.body))

			_, err := pullQuota(t.Context(), missingRuntimeTarget(t), tt.env)

			if err == nil {
				t.Fatal("pullQuota() error = nil, want error")
			}
			if got := errors.Is(err, fs.ErrNotExist); got != tt.wantRoute {
				t.Errorf("pullQuota() headless route taken = %v, want %v (error %v)", got, tt.wantRoute, err)
			}
			if spy.hits.Load() != tt.wantHits {
				t.Errorf("endpoint hits = %d, want %d", spy.hits.Load(), tt.wantHits)
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("pullQuota() error holds the token: %v", err)
			}
		})
	}
}

func TestPullQuota_CustomProviderSendsNothingAndEndsPulls(t *testing.T) {
	spy := serveQuota(t, http.StatusOK, nil)
	env := []string{"GH_TOKEN=token", providerBaseURLEnv + "=http://127.0.0.1:1"}
	var calls int
	puller := usage.NewPuller(quotaSource, time.Minute, func(ctx context.Context) ([]usage.WindowReading, error) {
		calls++
		return pullQuota(ctx, missingRuntimeTarget(t), env)
	})

	_, firstErr := puller.Pull(t.Context())
	_, secondErr := puller.Pull(t.Context())

	if !errors.Is(firstErr, usage.ErrPullUnsupported) || secondErr != nil || calls != 1 || spy.hits.Load() != 0 {
		t.Errorf("Pull() twice = %v, %v with %d calls and %d requests, want ErrPullUnsupported, nil, 1, 0", firstErr, secondErr, calls, spy.hits.Load())
	}
}

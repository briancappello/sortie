//go:build unix

package agentcore

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/waittest"
)

const (
	runtimeHomeVar    = "SORTIE_TEST_RUNTIME_HOME"
	quotedRuntimeFile = `my dir/it's "own".json`
	remoteContent     = "remote-token"
	localContent      = "local-token"

	// The lab shell sources ~/.bashrc, which is how the remote host sets a
	// variable that Sortie never forwards.
	remoteHomeInstall = `home=/home/sortie/rt
mkdir -p "$home/my dir"
printf '%s' ` + remoteContent + ` > "$home/credentials.json"
printf '%s' ` + remoteContent + ` > "$home/my dir/it's \"own\".json"
printf 'export ` + runtimeHomeVar + `=%s\n' "$home" > /home/sortie/.bashrc
chown -R sortie:sortie /home/sortie`
)

func TestLaunchTarget_ReadRuntimeFile_SSHLab(t *testing.T) {
	if reason := agenttest.SSHLabSkipReason(); reason != "" {
		t.Skip(reason)
	}

	workspace := t.TempDir()
	localHome := t.TempDir()
	for _, rel := range []string{"credentials.json", quotedRuntimeFile} {
		path := filepath.Join(localHome, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("MkdirAll(%q) = %v, want nil", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(localContent), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) = %v, want nil", path, err)
		}
	}

	lab := agenttest.StartSSHLab(t, agenttest.SSHLabParams{Install: remoteHomeInstall, Dirs: []string{workspace}})
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatalf("exec.LookPath(%q) = %v, want nil", "ssh", err)
	}
	target := LaunchTarget{Command: ssh, SSHHost: lab.Host, WorkspacePath: workspace, RemoteCommand: "true"}
	localEnv := []string{runtimeHomeVar + "=" + localHome}

	tests := []struct {
		name         string
		rel          string
		limit        int64
		want         string
		wantNotExist bool
		wantErr      bool
	}{
		{name: "remote home not the local file at the same path", rel: "credentials.json", limit: 1024, want: remoteContent},
		{name: "path with a space and quotes", rel: quotedRuntimeFile, limit: 1024, want: remoteContent},
		{name: "file exactly at the limit", rel: "credentials.json", limit: int64(len(remoteContent)), want: remoteContent},
		{name: "file above the limit", rel: "credentials.json", limit: int64(len(remoteContent)) - 1, wantErr: true},
		{name: "missing file", rel: "absent.json", limit: 1024, wantNotExist: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := target.ReadRuntimeFile(waittest.Context(t), localEnv, runtimeHomeVar, tt.rel, tt.limit)

			if gotNotExist := errors.Is(err, fs.ErrNotExist); gotNotExist != tt.wantNotExist {
				t.Errorf("ReadRuntimeFile(%q, limit %d) error = %v, errors.Is(fs.ErrNotExist) = %t, want %t", tt.rel, tt.limit, err, gotNotExist, tt.wantNotExist)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("ReadRuntimeFile(%q, limit %d) error = %v, want error %t", tt.rel, tt.limit, err, tt.wantErr)
			}
			if string(got) != tt.want {
				t.Errorf("ReadRuntimeFile(%q, limit %d) = %q, want %q", tt.rel, tt.limit, got, tt.want)
			}
		})
	}
}

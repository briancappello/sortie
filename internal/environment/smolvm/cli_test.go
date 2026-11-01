package smolvm

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseMachines_RecordedListing(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "ls-stopped.json"))
	if err != nil {
		t.Fatal(err)
	}
	machines, err := parseMachines(data)
	if err != nil {
		t.Fatalf("parseMachines() error = %v", err)
	}
	if len(machines) != 1 {
		t.Fatalf("parseMachines() = %d machines, want 1", len(machines))
	}
	m := machines[0]
	if m.Name != "ag1" || m.State != StateStopped || m.Running() || m.PID != nil {
		t.Errorf("machine = %+v, want ag1 stopped without a PID", m)
	}
	if m.Labels["sortie"] != "spike" || m.Mounts != 0 || !m.Network {
		t.Errorf("machine = %+v, want label sortie=spike, no mounts, network on", m)
	}

	data, err = os.ReadFile(filepath.Join("testdata", "ls-running.json"))
	if err != nil {
		t.Fatal(err)
	}
	machines, err = parseMachines(data)
	if err != nil {
		t.Fatalf("parseMachines() error = %v", err)
	}
	idx := slices.IndexFunc(machines, func(m Machine) bool { return m.Name == "ee-fixture" })
	if idx < 0 {
		t.Fatalf("parseMachines() = %+v, want ee-fixture", machines)
	}
	m = machines[idx]
	if !m.Running() || m.PID == nil || *m.PID != 2217213 {
		t.Errorf("machine = %+v, want running with PID 2217213", m)
	}
	if m.Labels["sortie.workspace"] != "VM-1" || m.Labels["sortie.test"] != "1" {
		t.Errorf("labels = %v, want sortie.workspace=VM-1 and sortie.test=1", m.Labels)
	}
	// Two directory mounts and one socket: smolvm counts only the
	// directories.
	if m.Mounts != 2 {
		t.Errorf("mounts = %d, want 2", m.Mounts)
	}
}

func TestParseMachines_RecordedListingMinimumVersion(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "ls-1.32.4.json"))
	if err != nil {
		t.Fatal(err)
	}
	machines, err := parseMachines(data)
	if err != nil {
		t.Fatalf("parseMachines() error = %v", err)
	}
	if len(machines) != 2 {
		t.Fatalf("parseMachines() = %d machines, want 2", len(machines))
	}
	running, created := machines[0], machines[1]
	if running.Name != "ls-fixture-running" || !running.Running() || running.PID == nil || *running.PID != 151903 {
		t.Errorf("machine = %+v, want ls-fixture-running running with PID 151903", running)
	}
	if running.Labels["sortie.workspace"] != "VM-1" || running.Labels["sortie.test"] != "1" {
		t.Errorf("labels = %v, want sortie.workspace=VM-1 and sortie.test=1", running.Labels)
	}
	// Two directory mounts and one socket: smolvm counts only the
	// directories. A credential binding alone turns the network on.
	if running.Mounts != 2 || !running.Network {
		t.Errorf("machine = %+v, want 2 mounts and network on", running)
	}
	// A machine never started is created, not stopped.
	if created.Name != "ls-fixture-stopped" || created.State != StateCreated || created.Running() || created.PID != nil {
		t.Errorf("machine = %+v, want ls-fixture-stopped created without a PID", created)
	}
	if created.Labels["sortie"] != "fixture" || created.Mounts != 0 || created.Network {
		t.Errorf("machine = %+v, want label sortie=fixture, no mounts, network off", created)
	}
}

func TestParseMachines_RejectsNonJSON(t *testing.T) {
	t.Parallel()

	if _, err := parseMachines([]byte("NAME STATE\nag1 stopped\n")); err == nil {
		t.Fatal("parseMachines() error = nil, want a parse error for table output")
	}
}

func TestCheckVersion(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"1.32.4", "1.32.5", "1.33.0", "2.0.0", "1.32.4+build.7", "10.0.0"} {
		if err := CheckVersion(v); err != nil {
			t.Errorf("CheckVersion(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"1.26.1", "1.32.3", "1.31.9", "0.40.0", "1.4.10", "1.32.4-rc.1"} {
		err := CheckVersion(v)
		if err == nil {
			t.Errorf("CheckVersion(%q) = nil, want a refusal", v)
			continue
		}
		if !strings.Contains(err.Error(), v) || !strings.Contains(err.Error(), "1.32.4") {
			t.Errorf("CheckVersion(%q) = %q, want it to name %q and 1.32.4", v, err, v)
		}
	}
	for _, v := range []string{"", "1.32", "v1.32.4", "1.32.4.1", "1.32.x", "01.32.4", "1.32.4 garbage", "99999999999999999999.0.0"} {
		err := CheckVersion(v)
		if err == nil {
			t.Errorf("CheckVersion(%q) = nil, want an unparsable version refused", v)
			continue
		}
		if !strings.Contains(err.Error(), "not a version") || !strings.Contains(err.Error(), "1.32.4") {
			t.Errorf("CheckVersion(%q) = %q, want it to say not a version and name 1.32.4", v, err)
		}
	}
}

func TestExecArgs(t *testing.T) {
	t.Parallel()

	got := ExecArgs("sortie-VM-1", ExecOptions{
		Workdir:     "/workspace",
		Env:         []string{"SORTIE_ISSUE_ID=1"},
		SecretEnv:   []string{"TOKEN=TOKEN"},
		Interactive: true,
	}, []string{"sh", "-c", "echo -- hi"})
	want := []string{
		"machine", "exec", "--error-format", "json", "--name", "sortie-VM-1", "-i", "-w", "/workspace",
		"-e", "SORTIE_ISSUE_ID=1", "--secret-env", "TOKEN=TOKEN", "--", "sh", "-c", "echo -- hi",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ExecArgs() = %q, want %q", got, want)
	}
}

func TestCreateArgs_CredentialHeader(t *testing.T) {
	t.Parallel()

	args, err := createArgs(CreateSpec{Name: "n", Pack: "/p", Credentials: []CredentialBinding{
		{Name: "a", Env: "A_KEY", Hosts: []string{"a.example"}},
		{Name: "b", Env: "B_KEY", Hosts: []string{"b.example", "*.b.example"}, Header: "X-Api-Key"},
	}})
	if err != nil {
		t.Fatalf("createArgs() error = %v", err)
	}
	want := []string{
		"machine", "create", "--error-format", "json", "--name", "n", "--from", "/p",
		"--credential", "a=A_KEY@a.example",
		"--credential", "b=B_KEY@b.example,*.b.example:set-header=X-Api-Key",
	}
	if !slices.Equal(args, want) {
		t.Errorf("createArgs() = %q, want %q", args, want)
	}
}

func TestCreateArgs_RefusesMountSeparatorInPath(t *testing.T) {
	t.Parallel()

	for _, spec := range []CreateSpec{
		{Name: "n", Pack: "/p", Volumes: []Volume{{Host: "/a:b", Guest: "/g"}}},
		{Name: "n", Pack: "/p", Sockets: []SocketMount{{Host: "/a:b/s.sock", Guest: "/g"}}},
		{Name: "n", Pack: "/p", Volumes: []Volume{{Host: "", Guest: "/g"}}},
	} {
		if args, err := createArgs(spec); err == nil {
			t.Errorf("createArgs(%+v) = %q, want a refusal", spec, args)
		}
	}
}

func TestCLIErrorMessage(t *testing.T) {
	t.Parallel()

	err := &CLIError{
		Args:     []string{"machine", "start"},
		Machine:  "sortie-VM-1",
		ExitCode: 1,
		Stderr:   "Starting machine 'sortie-VM-1'...\nError: vm not found: sortie-VM-1\n\x1b[33m WARN\x1b[0m unable to lock UID registry\n",
	}
	want := `smolvm machine start (machine "sortie-VM-1"): exit code 1: Error: vm not found: sortie-VM-1`
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if IsNotFound(err) {
		t.Error("IsNotFound() without a failure code = true, want false")
	}

	err.Code, err.Message = "NOT_FOUND", "vm not found: sortie-VM-1"
	want = `smolvm machine start (machine "sortie-VM-1"): exit code 1: NOT_FOUND: vm not found: sortie-VM-1`
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !IsNotFound(err) {
		t.Error("IsNotFound() = false, want true")
	}
	err.ExitCode = 2
	if IsNotFound(err) {
		t.Error("IsNotFound() with exit code 2 = true, want false")
	}
}

func TestParseErrorReport(t *testing.T) {
	t.Parallel()

	code, message, ok := parseErrorReport(`{"error":{"code":"CONFIG_ERROR","message":"config operation failed: resolve secret 'X': env_unset"}}`)
	if !ok || code != "CONFIG_ERROR" || message != "config operation failed: resolve secret 'X': env_unset" {
		t.Errorf("parseErrorReport() = %q, %q, %v", code, message, ok)
	}
	if _, _, ok := parseErrorReport(`{"error":{"code":"NOT_FOUND","message":""}}`); !ok {
		t.Error("parseErrorReport() with an empty message = false, want true")
	}
	for _, line := range []string{"", "Error: vm not found: x", "{", `{"error":null}`, `{"error":{}}`, `[{"error":{"code":"NOT_FOUND","message":"x"}}]`, `{"error":{"code":"NOT_FOUND","message":null}}`} {
		if _, _, ok := parseErrorReport(line); ok {
			t.Errorf("parseErrorReport(%q) = true, want false", line)
		}
	}
}

func TestCreateArgs_HostInputs(t *testing.T) {
	t.Parallel()

	args, err := createArgs(CreateSpec{
		Name: "n", Pack: "/p", Labels: []Label{{"k", "v"}},
		Nested: true, NetBackend: "virtio-net", AllowHosts: []string{"ghcr.io"},
		Volumes: []Volume{{Host: "/c", Guest: "/run/sortie/control"}, {Host: "/s", Guest: "/src", ReadOnly: true}, {Host: "/r/K", Guest: "/var/recorder"}},
	})
	if err != nil {
		t.Fatalf("createArgs() error = %v", err)
	}
	want := []string{
		"machine", "create", "--error-format", "json", "--name", "n", "--from", "/p",
		"--label", "k=v", "--nested", "--net-backend", "virtio-net",
		"--allow-host-pattern", "ghcr.io",
		"-v", "/c:/run/sortie/control", "-v", "/s:/src:ro", "-v", "/r/K:/var/recorder",
	}
	if !slices.Equal(args, want) {
		t.Errorf("createArgs() = %q, want %q", args, want)
	}
}

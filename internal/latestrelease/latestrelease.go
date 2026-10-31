// Package latestrelease finds the latest stable release of a dependency
// from its project's own release list, for the version tests that fail
// when Sortie falls behind. The tests run only with SORTIE_LATEST_TEST=1;
// [Require] skips them otherwise, before any network request.
package latestrelease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

// EnableEnv is the variable that enables the version tests.
const EnableEnv = "SORTIE_LATEST_TEST"

// Require skips t unless [EnableEnv] is "1".
func Require(t testing.TB) {
	t.Helper()
	if os.Getenv(EnableEnv) != "1" {
		t.Skip("skipping latest-release version test: set " + EnableEnv + "=1 to run it")
	}
}

// GitHubAPI is the base URL of the GitHub REST API.
const GitHubAPI = "https://api.github.com"

// maxBodyBytes bounds a release list response.
const maxBodyBytes = 16 << 20

// GitHub returns the highest stable release version of the GitHub
// repository repo ("owner/name"), without a leading "v". It reads the
// newest page of releases from api, skips drafts, prereleases, and tags
// that are not versions or carry a prerelease suffix, and sends GH_TOKEN
// as a bearer token when it is set. An error names the source.
func GitHub(ctx context.Context, client *http.Client, api, repo string) (string, error) {
	source := fmt.Sprintf("%s/repos/%s/releases", strings.TrimRight(api, "/"), repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source+"?per_page=100", nil)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", source, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token := os.Getenv("GH_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", source, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", source, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: HTTP %d: %s", source, resp.StatusCode, firstLine(body))
	}
	var releases []struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(body, &releases); err != nil {
		return "", fmt.Errorf("parse %s: %w", source, err)
	}
	best := ""
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		v := "v" + strings.TrimPrefix(r.TagName, "v")
		if !semver.IsValid(v) || semver.Prerelease(v) != "" {
			continue
		}
		if best == "" || semver.Compare(v, best) > 0 {
			best = v
		}
	}
	if best == "" {
		return "", fmt.Errorf("parse %s: no stable release among %d releases", source, len(releases))
	}
	return strings.TrimPrefix(semver.Canonical(best), "v"), nil
}

// Compare compares two versions written without a leading "v" by
// semantic version precedence. An invalid version orders before every
// valid one.
func Compare(a, b string) int {
	return semver.Compare("v"+a, "v"+b)
}

// Behind returns the message a version test fails with when ours is
// older than latest, or "" when it is not.
func Behind(dependency, ours, latest string) string {
	if Compare(ours, latest) >= 0 {
		return ""
	}
	return fmt.Sprintf("%s is behind: ours %s, latest %s", dependency, ours, latest)
}

// Module is one module a go.mod requires, with the update `go list -m
// -u` reports for it.
type Module struct {
	Path     string
	Version  string
	Indirect bool

	// Latest is the newer version the module proxy offers, or "" when
	// Version is the latest.
	Latest string
}

// GoModules returns every module the go.mod in dir requires, direct
// and indirect, with the update `go list -m -u -json all` reports for
// it. Modules of the build list that the go.mod does not require, which
// only other modules' requirements select, and modules replaced by a
// local directory are left out. An error names the source.
func GoModules(ctx context.Context, dir string) ([]Module, error) {
	gomod := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(gomod) //nolint:gosec // G304: the caller names its own module directory
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", gomod, err)
	}
	file, err := modfile.ParseLax(gomod, data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", gomod, err)
	}
	required := make(map[string]bool, len(file.Require))
	for _, r := range file.Require {
		required[r.Mod.Path] = true
	}

	source := fmt.Sprintf("go list -m -u -json all (in %s)", dir)
	cmd := exec.CommandContext(ctx, "go", "list", "-m", "-u", "-json", "all")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", source, err, strings.TrimSpace(stderr.String()))
	}
	return parseGoModules(out, source, required)
}

func parseGoModules(out []byte, source string, required map[string]bool) ([]Module, error) {
	type update struct {
		Version string
	}
	type replace struct {
		Path    string
		Version string
	}
	var mods []Module
	listed := make(map[string]bool, len(required))
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var m struct {
			Path     string
			Version  string
			Main     bool
			Indirect bool
			Update   *update
			Replace  *replace
			Error    *struct{ Err string }
		}
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("parse %s: %w", source, err)
		}
		if m.Error != nil {
			return nil, fmt.Errorf("%s: module %s: %s", source, m.Path, m.Error.Err)
		}
		listed[m.Path] = true
		if m.Main || !required[m.Path] || (m.Replace != nil && m.Replace.Version == "") {
			continue
		}
		mod := Module{Path: m.Path, Version: m.Version, Indirect: m.Indirect}
		if m.Update != nil {
			mod.Latest = m.Update.Version
		}
		mods = append(mods, mod)
	}
	for path := range required {
		if !listed[path] {
			return nil, fmt.Errorf("parse %s: required module %s is not listed", source, path)
		}
	}
	return mods, nil
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if len(line) > 200 {
		line = line[:200] + "..."
	}
	return line
}

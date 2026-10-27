package agentcore

import (
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

// ParseRuntimeVersion extracts the version and major number from a runtime's
// --version output. ok is false when no whitespace-separated field of the
// first readable line is a full semantic version once one leading "v" and
// one trailing "." are dropped.
func ParseRuntimeVersion(out []byte) (version string, major int, ok bool) {
	for field := range strings.FieldsSeq(FirstReadableLine(out)) {
		candidate := strings.TrimSuffix(strings.TrimPrefix(field, "v"), ".")
		prefixed := "v" + candidate
		if !isFullSemver(prefixed) {
			continue
		}
		majorInt, err := strconv.Atoi(strings.TrimPrefix(semver.Major(prefixed), "v"))
		if err != nil {
			return "", 0, false
		}
		return candidate, majorInt, true
	}
	return "", 0, false
}

// isFullSemver reports whether version, with its leading "v", is valid
// semver that spells out major, minor and patch. semver.IsValid alone
// also accepts shorthand such as "v1.18".
func isFullSemver(version string) bool {
	return semver.Canonical(version) == strings.TrimSuffix(version, semver.Build(version))
}

// FirstReadableLine returns the first line of out that is non-empty after
// [SanitizeLine] and trimming, or "" when every line is empty.
func FirstReadableLine(out []byte) string {
	for raw := range strings.SplitSeq(string(out), "\n") {
		line := strings.TrimSpace(SanitizeLine(raw))
		if line != "" {
			return line
		}
	}
	return ""
}

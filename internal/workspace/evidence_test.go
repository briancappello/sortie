package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEvidence(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	tests := []struct {
		name, doc, want string
	}{
		{"commit", `{"kind":"commit","sha":"` + sha + `"}`, ""},
		{"plan key", `{"kind":"plan_key","plan_key":"payments/http-client","note":"retries exist"}`, ""},
		{"short sha", `{"kind":"commit","sha":"abc123"}`, EvidenceNoCitation},
		{"uppercase sha", `{"kind":"commit","sha":"` + strings.ToUpper(sha) + `"}`, EvidenceNoCitation},
		{"two citations", `{"kind":"commit","sha":"` + sha + `","plan_key":"a/b"}`, EvidenceNoCitation},
		{"bad plan key", `{"kind":"plan_key","plan_key":"Billing X"}`, EvidenceNoCitation},
		{"no kind", `{"sha":"` + sha + `"}`, EvidenceNoCitation},
		{"unknown kind", `{"kind":"vibes"}`, EvidenceNoCitation},
		{"unknown field", `{"kind":"commit","sha":"` + sha + `","pr":1}`, EvidenceUnparsed},
		{"not json", `commit ` + sha, EvidenceUnparsed},
		{"two documents", `{"kind":"commit","sha":"` + sha + `"} {}`, EvidenceUnparsed},
		{"long note", `{"kind":"commit","sha":"` + sha + `","note":"` + strings.Repeat("x", 2001) + `"}`, EvidenceUnparsed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, got := ParseEvidence([]byte(tt.doc)); got != tt.want {
				t.Errorf("ParseEvidence(%s) reason = %q, want %q", tt.doc, got, tt.want)
			}
		})
	}
}

func TestReadEvidenceFile_Refusals(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".sortie"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, why := ReadEvidenceFile(ws, nil); why != EvidenceMissing {
		t.Errorf("absent file reason = %q, want %q", why, EvidenceMissing)
	}

	big := filepath.Join(ws, ".sortie", EvidenceFileName)
	if err := os.WriteFile(big, []byte(strings.Repeat(" ", 5000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, why := ReadEvidenceFile(ws, nil); why != EvidenceUnparsed {
		t.Errorf("oversize file reason = %q, want %q", why, EvidenceUnparsed)
	}

	_ = os.Remove(big)
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"kind":"commit","sha":"`+strings.Repeat("b", 40)+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, big); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, why := ReadEvidenceFile(ws, nil); why != EvidenceUnparsed {
		t.Errorf("symlinked file reason = %q, want %q (refused)", why, EvidenceUnparsed)
	}

	CleanupEvidenceFile(ws, nil)
	if _, err := os.Lstat(target); err != nil {
		t.Errorf("cleanup followed the link and removed its target: %v", err)
	}
}

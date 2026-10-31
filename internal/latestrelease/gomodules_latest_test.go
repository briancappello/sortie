package latestrelease

import (
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/waittest"
)

// TestLatest_GoModules fails for every module a go.mod of this
// repository requires that is behind its latest release.
func TestLatest_GoModules(t *testing.T) {
	Require(t)

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".", filepath.Join("tools", "qualify")} {
		dir := filepath.Join(root, rel)
		t.Run(rel, func(t *testing.T) {
			mods, err := GoModules(waittest.Context(t), dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range mods {
				if m.Latest != "" {
					t.Errorf("go module %s (in %s) is behind: ours %s, latest %s", m.Path, dir, m.Version, m.Latest)
				}
			}
		})
	}
}

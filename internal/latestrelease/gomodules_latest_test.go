package latestrelease

import (
	"context"
	"path/filepath"
	"testing"
	"time"
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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			mods, err := GoModules(ctx, dir)
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

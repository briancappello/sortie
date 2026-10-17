package main

import (
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

// writeCapableTrackerKinds lists the tracker kinds that declare write
// capabilities. Every other registered kind must declare none.
var writeCapableTrackerKinds = []string{"huly"}

// TestEveryTrackerKindDeclaresOnlyImplementedWriteCapabilities walks
// every tracker kind main registers. Each declaration passes the
// registry's check, and a kind outside writeCapableTrackerKinds declares
// no write capability at all.
func TestEveryTrackerKindDeclaresOnlyImplementedWriteCapabilities(t *testing.T) {
	t.Parallel()

	kinds := registry.Trackers.Kinds()
	if len(kinds) == 0 {
		t.Fatal("registry.Trackers.Kinds() is empty; nothing would be asserted")
	}
	for _, kind := range kinds {
		meta, _ := registry.Trackers.Meta(kind)
		if err := registry.CheckTrackerMeta(meta); err != nil {
			t.Errorf("tracker kind %q: %v", kind, err)
		}
		if !slices.Contains(writeCapableTrackerKinds, kind) && len(meta.WriteCapabilities) != 0 {
			t.Errorf("tracker kind %q declares write capabilities %v; add it to writeCapableTrackerKinds", kind, meta.WriteCapabilities)
		}
	}
}

package persistence

import (
	"context"
	"strings"
	"testing"
)

// Fork migrations live in their own range, after every upstream one.
func TestMigrations_ForkRangeStartsAt1001(t *testing.T) {
	t.Parallel()
	for _, m := range migrations {
		if m.Version > 24 && m.Version < 1001 {
			t.Errorf("migration %d (%s) is between upstream's range and the fork range", m.Version, m.Description)
		}
	}
}

// recordOldForkNumbering builds the database an earlier fork build left:
// upstream migrations 1 to 24, then the fork's three run_history columns
// recorded as versions 25 to 27.
func recordOldForkNumbering(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	migrateToVersion(t, s, 24)
	for old, current := range map[int]int{25: 1001, 26: 1002, 27: 1003} {
		for _, m := range migrations {
			if m.Version != current {
				continue
			}
			if err := s.applyMigration(ctx, Migration{Version: old, Description: m.Description, SQL: m.SQL}); err != nil {
				t.Fatalf("apply fork migration %d as %d: %v", current, old, err)
			}
		}
	}
}

func TestMigrate_RefusesOldForkNumbering(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	recordOldForkNumbering(t, s)

	err := s.Migrate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "delete the database file") {
		t.Fatalf("Migrate() = %v, want the old-fork-numbering refusal", err)
	}
}

// An upstream database at version 25 or later, without the fork's columns,
// is not mistaken for an old fork database.
func TestMigrate_UpstreamVersion25IsNotRefused(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	migrateToVersion(t, s, 24)
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO schema_migrations (version, applied_at) VALUES (25, '2026-10-16T00:00:00Z')`); err != nil {
		t.Fatalf("record upstream version 25: %v", err)
	}

	migrateOrFatal(t, s)
	if !hasColumn(t, s, "run_history", "held_ms") {
		t.Error("fork migration 1001 did not run on an upstream database")
	}
}

// A database migrated with the fork range migrates again with no change.
func TestMigrate_ForkRangeIsIdempotent(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	migrateOrFatal(t, s)
	migrateOrFatal(t, s)
}

func hasColumn(t *testing.T, s *Store, table, column string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	return n == 1
}

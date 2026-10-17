package persistence

import _ "embed" // enables //go:embed directives for SQL migration files

// Migration is a single numbered schema migration applied sequentially
// by [Store.Migrate]. SQL may contain multiple semicolon-separated
// statements.
type Migration struct {
	Version     int
	Description string
	SQL         string
}

//go:embed sql/001_initial.sql
var migration001SQL string

//go:embed sql/002_extended_token_metrics.sql
var migration002SQL string

//go:embed sql/003_workflow_file.sql
var migration003SQL string

//go:embed sql/004_run_history_issue_id_index.sql
var migration004SQL string

//go:embed sql/005_run_history_turns.sql
var migration005SQL string

//go:embed sql/006_display_identifier.sql
var migration006SQL string

//go:embed sql/007_self_review.sql
var migration007SQL string

//go:embed sql/008_reaction_fingerprints.sql
var migration008SQL string

//go:embed sql/009_retry_session_id.sql
var migration009SQL string

//go:embed sql/010_dispatch_rule_routing.sql
var migration010SQL string

//go:embed sql/011_run_history_token_columns.sql
var migration011SQL string

//go:embed sql/012_run_history_tokens_measured.sql
var migration012SQL string

//go:embed sql/013_handoff_absence_resets.sql
var migration013SQL string

//go:embed sql/014_parked_issues.sql
var migration014SQL string

//go:embed sql/015_budget_hold_notices.sql
var migration015SQL string

//go:embed sql/016_session_metadata_requests_measured.sql
var migration016SQL string

//go:embed sql/017_session_metadata_dispatch_id.sql
var migration017SQL string

//go:embed sql/018_aggregate_metrics_unmeasured_sessions.sql
var migration018SQL string

//go:embed sql/019_run_history_unaccounted_turns.sql
var migration019SQL string

//go:embed sql/020_cache_write_tokens.sql
var migration020SQL string

//go:embed sql/021_run_history_configured_settings.sql
var migration021SQL string

//go:embed sql/022_reaction_handoffs.sql
var migration022SQL string

//go:embed sql/023_stage_hops.sql
var migration023SQL string

//go:embed sql/024_stage_chains.sql
var migration024SQL string

//go:embed sql/1001_run_history_held_ms.sql
var migration1001SQL string

//go:embed sql/1002_run_history_environment_image.sql
var migration1002SQL string

//go:embed sql/1003_run_history_exported_commit.sql
var migration1003SQL string

//go:embed sql/1004_plan_satisfaction.sql
var migration1004SQL string

// migrations lists upstream's migrations, numbered from 1 in upstream's
// sequence, then the fork's, numbered from 1001. Upstream merges add lower
// numbers, so a database that already ran fork migrations applies new
// upstream migrations after them. Fork migrations must therefore only add fork-owned tables and
// columns, and never depend on, or change, anything an upstream migration
// may add later.
var migrations = []Migration{
	{Version: 1, Description: "core persistence tables", SQL: migration001SQL},
	{Version: 2, Description: "extended token metrics", SQL: migration002SQL},
	{Version: 3, Description: "workflow file in run history", SQL: migration003SQL},
	{Version: 4, Description: "run_history issue_id index", SQL: migration004SQL},
	{Version: 5, Description: "turns completed in run history", SQL: migration005SQL},
	{Version: 6, Description: "display identifier in run history", SQL: migration006SQL},
	{Version: 7, Description: "self-review metadata in run history", SQL: migration007SQL},
	{Version: 8, Description: "reaction fingerprints for cross-restart dedup", SQL: migration008SQL},
	{Version: 9, Description: "session ID in retry entries for cross-retry resume", SQL: migration009SQL},
	{Version: 10, Description: "dispatch rule routing columns on retry_entries and run_history", SQL: migration010SQL},
	{Version: 11, Description: "run_history token columns", SQL: migration011SQL},
	{Version: 12, Description: "tokens_measured column on run_history", SQL: migration012SQL},
	{Version: 13, Description: "handoff absence sequence reset points", SQL: migration013SQL},
	{Version: 14, Description: "parked issues held out of primary dispatch", SQL: migration014SQL},
	{Version: 15, Description: "budget hold notices for cross-restart tracker-comment dedup", SQL: migration015SQL},
	{Version: 16, Description: "api_requests_measured column on session_metadata", SQL: migration016SQL},
	{Version: 17, Description: "dispatch_id column on session_metadata", SQL: migration017SQL},
	{Version: 18, Description: "unmeasured_sessions column on aggregate_metrics", SQL: migration018SQL},
	{Version: 19, Description: "unaccounted_turns column on run_history", SQL: migration019SQL},
	{Version: 20, Description: "cache_write_tokens column on run_history, session_metadata, and aggregate_metrics", SQL: migration020SQL},
	{Version: 21, Description: "configured model, configured effort, and reported model on run_history", SQL: migration021SQL},
	{Version: 22, Description: "review comments each issue's runs were given", SQL: migration022SQL},
	{Version: 23, Description: "stage hop counts and the previous stage on retry entries and run history", SQL: migration023SQL},
	{Version: 24, Description: "stage chain identifier and hop result on run history, retry entries, and stage hops", SQL: migration024SQL},
	{Version: 1001, Description: "held_ms column on run_history", SQL: migration1001SQL},
	{Version: 1002, Description: "environment_image_sha256 column on run_history", SQL: migration1002SQL},
	{Version: 1003, Description: "exported_commit column on run_history", SQL: migration1003SQL},
	{Version: 1004, Description: "plan_satisfaction: what satisfies a plan blocker", SQL: migration1004SQL},
}

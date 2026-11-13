package repository_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrationIndexes is a DB-free guard that the hot claim/poll/list
// indexes exist in the applied Atlas migrations (PERF-3 plus the
// claim/node follow-up batch). It globs the version files
// (filename-agnostic) and asserts each expected CREATE INDEX statement —
// with its columns in order and, for partials, its WHERE predicate — plus
// the DROPs of the standalone indexes the composites subsume.
//
// If this fails after a model-tag change, regenerate with `task migrate-diff`
// (raw `atlas migrate diff ...` from Taskfile.yaml when task is unavailable).
func TestMigrationIndexes(t *testing.T) {
	files, err := filepath.Glob("../../../migrations/versions/*.sql")
	if err != nil {
		t.Fatalf("glob versions: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no migration versions found; run `task migrate-diff` to generate them")
	}
	var b strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	ddl := b.String()

	// Tokens per index must appear in order, starting at the index name, so
	// composite column order and partial predicates are covered, not just the
	// index name.
	want := []struct {
		name   string
		tokens []string
	}{
		{"idx_workflow_instances_claim", []string{
			`"idx_workflow_instances_claim"`,
			`"status"`, `"waiting_reason"`, `"lease_expiry"`, `"updated_at"`,
		}},
		{"idx_workflow_instances_termination_pending", []string{
			`"idx_workflow_instances_termination_pending"`,
			`"termination_pending"`, `WHERE`, `termination_pending = true`,
		}},
		{"idx_status_update_outbox_ready", []string{
			`"idx_status_update_outbox_ready"`,
			`"next_attempt_at"`, `WHERE`, `delivered_at IS NULL`, `dead_at IS NULL`,
		}},
		{"idx_cron_schedules_enabled", []string{
			`"idx_cron_schedules_enabled"`, `"enabled"`,
		}},
		{"idx_workflow_instances_created_by", []string{
			`"idx_workflow_instances_created_by"`,
			`"created_by"`, `"created_at"`,
		}},
		{"idx_workflow_instances_delay", []string{
			`"idx_workflow_instances_delay"`,
			`"status"`, `"waiting_reason"`, `"run_at"`, `WHERE`, `waiting_reason = 'delay'`,
		}},
		{"idx_parallel_branches_delay", []string{
			`"idx_parallel_branches_delay"`,
			`"status"`, `"waiting_reason"`, `"run_at"`, `WHERE`, `waiting_reason = 'delay'`,
		}},
		{"idx_parallel_branches_claim", []string{
			`"idx_parallel_branches_claim"`,
			`"status"`, `"waiting_reason"`, `"lease_expiry"`, `"updated_at"`,
		}},
		{"idx_node_instances_instance_node", []string{
			`"idx_node_instances_instance_node"`,
			`"workflow_instance_id"`, `"node_id"`, `"status"`,
		}},
		{"idx_node_instances_branch_node", []string{
			`"idx_node_instances_branch_node"`,
			`"branch_id"`, `"node_id"`, `"status"`,
		}},
		{"idx_workflow_instance_events_instance", []string{
			`"idx_workflow_instance_events_instance"`,
			`"workflow_instance_id"`, `"created_at"`, `"id"`,
		}},
		{"idx_workflow_instances_running", []string{
			`"idx_workflow_instances_running"`,
			`"lease_expiry"`, `WHERE`, `status = 'running'`,
		}},
		{"idx_parallel_branches_running", []string{
			`"idx_parallel_branches_running"`,
			`"lease_expiry"`, `WHERE`, `status = 'running'`,
		}},
		{"idx_node_definitions_lineage_version", []string{
			`"idx_node_definitions_lineage_version"`,
			`"lineage_id"`, `"version" DESC`,
		}},
		{"idx_workflow_definitions_lineage_version", []string{
			`"idx_workflow_definitions_lineage_version"`,
			`"lineage_id"`, `"version" DESC`,
		}},
		// Anchored on the DROP: the token index name also appears in
		// the older full-index CREATE, so the search must start at the
		// replacement.
		{"idx_users_verification_token", []string{
			`DROP INDEX "public"."idx_users_verification_token"`,
			`"idx_users_verification_token"`,
			`"verification_token_hash"`, `WHERE`, `<> ''`,
		}},
		{"idx_users_email_trgm", []string{
			`"idx_users_email_trgm"`,
			`USING GIN`, `"email"`, `gin_trgm_ops`,
		}},
		{"idx_users_name_trgm", []string{
			`"idx_users_name_trgm"`,
			`USING GIN`, `"name"`, `gin_trgm_ops`,
		}},
	}
	for _, w := range want {
		pos := 0
		for _, tok := range w.tokens {
			i := strings.Index(ddl[pos:], tok)
			if i < 0 {
				t.Errorf("migrations lack %s (missing %q after %d bytes); regenerate with `task migrate-diff`", w.name, tok, pos)
				break
			}
			pos += i + len(tok)
		}
	}

	// The claim composite's status prefix covers status-only predicates, so
	// the old standalone index must be dropped, not kept alongside.
	drops := []string{
		`DROP INDEX "public"."idx_workflow_instances_status";`,
		`DROP INDEX "public"."idx_parallel_branches_status";`,
		`DROP INDEX "public"."idx_parallel_branches_lease_expiry";`,
		`DROP INDEX "public"."idx_node_context_history_workflow_instance_id";`,
		`DROP INDEX "public"."idx_node_context_history_occurrence_id";`,
		`DROP INDEX "public"."idx_node_context_history_superseded";`,
		`DROP INDEX "public"."idx_input_deliveries_workflow_instance_id";`,
		`DROP INDEX "public"."idx_node_definitions_lineage_id";`,
		`DROP INDEX "public"."idx_workflow_definitions_lineage_id";`,
	}
	for _, drop := range drops {
		if !strings.Contains(ddl, drop) {
			t.Errorf("migrations lack %s; regenerate with `task migrate-diff`", drop)
		}
	}

	// The trigram opclass needs its extension on every database the
	// migrations touch. It is hand-added (Atlas CE never emits it), so
	// guard the line itself rather than anything generated.
	if !strings.Contains(ddl, `CREATE EXTENSION IF NOT EXISTS "pg_trgm";`) {
		t.Error(`migrations lack CREATE EXTENSION IF NOT EXISTS "pg_trgm"`)
	}
}

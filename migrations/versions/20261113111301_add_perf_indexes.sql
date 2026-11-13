-- Create index "idx_cron_schedules_created_by" to table: "cron_schedules"
CREATE INDEX "idx_cron_schedules_created_by" ON "public"."cron_schedules" ("created_by");
-- Create index "idx_cron_schedules_updated_by" to table: "cron_schedules"
CREATE INDEX "idx_cron_schedules_updated_by" ON "public"."cron_schedules" ("updated_by");
-- Drop index "idx_input_deliveries_workflow_instance_id" from table: "input_deliveries"
DROP INDEX "public"."idx_input_deliveries_workflow_instance_id";
-- Drop index "idx_node_context_history_occurrence_id" from table: "node_context_history"
DROP INDEX "public"."idx_node_context_history_occurrence_id";
-- Drop index "idx_node_context_history_superseded" from table: "node_context_history"
DROP INDEX "public"."idx_node_context_history_superseded";
-- Drop index "idx_node_context_history_workflow_instance_id" from table: "node_context_history"
DROP INDEX "public"."idx_node_context_history_workflow_instance_id";
-- Drop index "idx_node_definitions_lineage_id" from table: "node_definitions"
DROP INDEX "public"."idx_node_definitions_lineage_id";
-- Create index "idx_node_definitions_created_by" to table: "node_definitions"
CREATE INDEX "idx_node_definitions_created_by" ON "public"."node_definitions" ("created_by");
-- Create index "idx_node_definitions_lineage_version" to table: "node_definitions"
CREATE INDEX "idx_node_definitions_lineage_version" ON "public"."node_definitions" ("lineage_id", "version" DESC);
-- Create index "idx_node_definitions_updated_by" to table: "node_definitions"
CREATE INDEX "idx_node_definitions_updated_by" ON "public"."node_definitions" ("updated_by");
-- Create index "idx_node_instances_branch_node" to table: "node_instances"
CREATE INDEX "idx_node_instances_branch_node" ON "public"."node_instances" ("branch_id", "node_id", "status");
-- Create index "idx_node_instances_instance_node" to table: "node_instances"
CREATE INDEX "idx_node_instances_instance_node" ON "public"."node_instances" ("workflow_instance_id", "node_id", "status");
-- Create index "idx_node_instances_node_definition_id" to table: "node_instances"
CREATE INDEX "idx_node_instances_node_definition_id" ON "public"."node_instances" ("node_definition_id");
-- Drop index "idx_parallel_branches_lease_expiry" from table: "parallel_branches"
DROP INDEX "public"."idx_parallel_branches_lease_expiry";
-- Drop index "idx_parallel_branches_status" from table: "parallel_branches"
DROP INDEX "public"."idx_parallel_branches_status";
-- Create index "idx_parallel_branches_claim" to table: "parallel_branches"
CREATE INDEX "idx_parallel_branches_claim" ON "public"."parallel_branches" ("status", "waiting_reason", "lease_expiry", "updated_at");
-- Create index "idx_parallel_branches_running" to table: "parallel_branches"
CREATE INDEX "idx_parallel_branches_running" ON "public"."parallel_branches" ("lease_expiry") WHERE (status = 'running'::text);
-- Drop index "idx_users_verification_token" from table: "users"
DROP INDEX "public"."idx_users_verification_token";
-- Create index "idx_users_verification_token" to table: "users"
CREATE INDEX "idx_users_verification_token" ON "public"."users" ("verification_token_hash") WHERE (verification_token_hash <> ''::text);
-- Drop index "idx_workflow_definitions_lineage_id" from table: "workflow_definitions"
DROP INDEX "public"."idx_workflow_definitions_lineage_id";
-- Create index "idx_workflow_definitions_created_by" to table: "workflow_definitions"
CREATE INDEX "idx_workflow_definitions_created_by" ON "public"."workflow_definitions" ("created_by");
-- Create index "idx_workflow_definitions_lineage_version" to table: "workflow_definitions"
CREATE INDEX "idx_workflow_definitions_lineage_version" ON "public"."workflow_definitions" ("lineage_id", "version" DESC);
-- Create index "idx_workflow_definitions_updated_by" to table: "workflow_definitions"
CREATE INDEX "idx_workflow_definitions_updated_by" ON "public"."workflow_definitions" ("updated_by");
-- Create index "idx_workflow_instance_events_created_by" to table: "workflow_instance_events"
CREATE INDEX "idx_workflow_instance_events_created_by" ON "public"."workflow_instance_events" ("created_by");
-- Create index "idx_workflow_instance_events_instance" to table: "workflow_instance_events"
CREATE INDEX "idx_workflow_instance_events_instance" ON "public"."workflow_instance_events" ("workflow_instance_id", "created_at", "id");
-- Create index "idx_workflow_instances_running" to table: "workflow_instances"
CREATE INDEX "idx_workflow_instances_running" ON "public"."workflow_instances" ("lease_expiry") WHERE (status = 'running'::text);

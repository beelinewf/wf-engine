-- Modify "workflow_instances" table
ALTER TABLE "public"."workflow_instances" ADD COLUMN "run_at" timestamptz NULL;
-- Create index "idx_workflow_instances_delay" to table: "workflow_instances"
CREATE INDEX "idx_workflow_instances_delay" ON "public"."workflow_instances" ("status", "waiting_reason", "run_at") WHERE (waiting_reason = 'delay'::text);

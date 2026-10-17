-- Modify "workflow_instances" table
ALTER TABLE "public"."workflow_instances" ADD COLUMN "form_snapshot" jsonb NULL;

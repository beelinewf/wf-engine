-- Modify "workflow_instances" table
ALTER TABLE "public"."workflow_instances" ADD COLUMN "migrated_from" uuid NULL, ADD COLUMN "superseded_by" uuid NULL;

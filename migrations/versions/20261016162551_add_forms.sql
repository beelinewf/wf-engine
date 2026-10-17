-- Create "forms" table
CREATE TABLE "public"."forms" (
  "id" uuid NOT NULL,
  "name" text NOT NULL,
  "schema" jsonb NOT NULL,
  "ui" jsonb NULL,
  "created_at" timestamptz NOT NULL,
  "updated_at" timestamptz NOT NULL,
  PRIMARY KEY ("id")
);
-- Create "node_definition_form_refs" table
CREATE TABLE "public"."node_definition_form_refs" (
  "node_definition_id" uuid NOT NULL,
  "form_id" uuid NOT NULL,
  "created_at" timestamptz NOT NULL,
  PRIMARY KEY ("node_definition_id", "form_id"),
  CONSTRAINT "fk_node_def_form_refs_form" FOREIGN KEY ("form_id") REFERENCES "public"."forms" ("id") ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT "fk_node_def_form_refs_node_definition" FOREIGN KEY ("node_definition_id") REFERENCES "public"."node_definitions" ("id") ON UPDATE RESTRICT ON DELETE RESTRICT
);
-- Create index "idx_node_definition_form_refs_form_id" to table: "node_definition_form_refs"
CREATE INDEX "idx_node_definition_form_refs_form_id" ON "public"."node_definition_form_refs" ("form_id");
-- Create "workflow_definition_form_refs" table
CREATE TABLE "public"."workflow_definition_form_refs" (
  "workflow_definition_id" uuid NOT NULL,
  "form_id" uuid NOT NULL,
  "created_at" timestamptz NOT NULL,
  PRIMARY KEY ("workflow_definition_id", "form_id"),
  CONSTRAINT "fk_wf_def_form_refs_form" FOREIGN KEY ("form_id") REFERENCES "public"."forms" ("id") ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT "fk_wf_def_form_refs_workflow_definition" FOREIGN KEY ("workflow_definition_id") REFERENCES "public"."workflow_definitions" ("id") ON UPDATE RESTRICT ON DELETE RESTRICT
);
-- Create index "idx_workflow_definition_form_refs_form_id" to table: "workflow_definition_form_refs"
CREATE INDEX "idx_workflow_definition_form_refs_form_id" ON "public"."workflow_definition_form_refs" ("form_id");

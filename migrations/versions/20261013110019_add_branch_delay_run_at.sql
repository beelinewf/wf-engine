-- Modify "parallel_branches" table
ALTER TABLE "public"."parallel_branches" ADD COLUMN "run_at" timestamptz NULL;
-- Create index "idx_parallel_branches_delay" to table: "parallel_branches"
CREATE INDEX "idx_parallel_branches_delay" ON "public"."parallel_branches" ("status", "waiting_reason", "run_at") WHERE (waiting_reason = 'delay'::text);

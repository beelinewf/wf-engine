-- pg_trgm ships the gin_trgm_ops opclass the indexes below need. It is
-- created here by hand: Atlas CE cannot carry extensions in the desired
-- schema, so `migrate diff` never emits this line.
CREATE EXTENSION IF NOT EXISTS "pg_trgm";
-- Create index "idx_users_email_trgm" to table: "users"
CREATE INDEX "idx_users_email_trgm" ON "public"."users" USING GIN ("email" public.gin_trgm_ops);
-- Create index "idx_users_name_trgm" to table: "users"
CREATE INDEX "idx_users_name_trgm" ON "public"."users" USING GIN ("name" public.gin_trgm_ops);

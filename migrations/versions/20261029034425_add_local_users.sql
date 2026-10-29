-- Modify "users" table
ALTER TABLE "public"."users" ADD COLUMN "password_hash" text NOT NULL DEFAULT '', ADD COLUMN "is_active" boolean NOT NULL DEFAULT true, ADD COLUMN "email_verified_at" timestamptz NULL, ADD COLUMN "verification_token_hash" text NOT NULL DEFAULT '', ADD COLUMN "verification_expires_at" timestamptz NULL, ADD COLUMN "verification_sent_at" timestamptz NULL;
-- Create index "idx_users_verification_token" to table: "users"
CREATE INDEX "idx_users_verification_token" ON "public"."users" ("verification_token_hash");
-- Create index "uq_users_local_email" to table: "users"
CREATE UNIQUE INDEX "uq_users_local_email" ON "public"."users" ("email") WHERE (issuer = 'local'::text);
-- Create "user_roles" table
CREATE TABLE "public"."user_roles" (
  "user_id" uuid NOT NULL,
  "role" text NOT NULL,
  "created_at" timestamptz NOT NULL,
  "updated_at" timestamptz NOT NULL,
  PRIMARY KEY ("user_id", "role"),
  CONSTRAINT "fk_user_roles_role" FOREIGN KEY ("role") REFERENCES "public"."roles" ("name") ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT "fk_user_roles_user" FOREIGN KEY ("user_id") REFERENCES "public"."users" ("id") ON UPDATE RESTRICT ON DELETE RESTRICT
);
-- Backfill: every pre-existing user proved its mailbox already (OIDC
-- callers to their provider; the system user never logs in), so mark
-- them verified. Only future pending self-registrations stay NULL.
UPDATE "public"."users" SET "email_verified_at" = now() WHERE "email_verified_at" IS NULL;

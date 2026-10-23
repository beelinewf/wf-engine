-- Modify "forms" table
ALTER TABLE "public"."forms" ADD COLUMN "content_type" text NOT NULL DEFAULT 'application/json';

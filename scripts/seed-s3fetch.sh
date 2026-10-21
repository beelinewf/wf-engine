#!/usr/bin/env bash
# Seed s3fetch example custom-node definitions (pipe + presign).
#
# Background: the s3fetch type registers in-process at startup
# (pkg/customnode/s3fetch init via pkg/customnode/all blank import).
# GET /v1/node/definition lists only rows in the node_definitions table,
# so fresh stacks return empty until definitions are POSTed. This script
# POSTs ready-to-use s3fetch definitions, mirroring scripts/seed.sh.
#
# Usage: scripts/seed-s3fetch.sh [BASE_URL]

set -euo pipefail

BASE_URL="${1:-http://localhost:8080}"

fail() {
  echo "seed-s3fetch: FAIL: $*" >&2
  exit 1
}

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"

# api_curl wraps curl with the optional API-key header: when BEELINEWF_API_TOKEN
# is set, requests authenticate as the service principal, otherwise this is
# plain curl (no-auth deployments keep working unchanged).
api_curl() {
  if [[ -n "${BEELINEWF_API_TOKEN:-}" ]]; then
    curl -H "X-Api-Token: $BEELINEWF_API_TOKEN" "$@"
  else
    curl "$@"
  fi
}

node_definitions=$(cat <<'JSON'
{
  "s3fetch_pipe": {
    "name": "fetch-report-to-s3",
    "type": "s3fetch",
    "content": {
      "type": "s3fetch",
      "config": {
        "operation": "pipe",
        "endpoint": "{{ env.BEELINEWF_S3_ENDPOINT }}",
        "region": "us-east-1",
        "bucket": "reports",
        "key": "seed/pipe-probe.pdf",
        "source_url": "https://example.com/report.pdf",
        "expiry_seconds": 3600,
        "use_ssl": false,
        "access_key": "{{ env.BEELINEWF_S3_ACCESS_KEY }}",
        "secret_key": "{{ env.BEELINEWF_S3_SECRET_KEY }}"
      },
      "output_property": "report",
      "timeout": "120s"
    }
  },
  "s3fetch_presign": {
    "name": "share-report-from-s3",
    "type": "s3fetch",
    "content": {
      "type": "s3fetch",
      "config": {
        "operation": "presign",
        "endpoint": "{{ env.BEELINEWF_S3_ENDPOINT }}",
        "region": "us-east-1",
        "bucket": "reports",
        "key": "seed/pipe-probe.pdf",
        "expiry_seconds": 3600,
        "use_ssl": false,
        "access_key": "{{ env.BEELINEWF_S3_ACCESS_KEY }}",
        "secret_key": "{{ env.BEELINEWF_S3_SECRET_KEY }}"
      },
      "output_property": "report",
      "timeout": "30s"
    }
  }
}
JSON
)

node_definition_ids='{}'

while IFS= read -r key; do
  payload=$(jq -ce --arg key "$key" '.[$key]' <<<"$node_definitions") \
    || fail "read node definition $key"

  response=$(api_curl -fsS -X POST \
    -H 'Content-Type: application/json' \
    --data-binary "$payload" \
    "$BASE_URL/v1/node/definition") \
    || fail "create node definition $key"

  id=$(jq -er '.id' <<<"$response") \
    || fail "parse node definition id for $key"
  name=$(jq -er '.name' <<<"$payload") \
    || fail "parse node definition name for $key"
  schema_type=$(jq -er '.schema.properties.type.const // empty' <<<"$response") \
    || fail "node definition $key response missing schema"

  node_definition_ids=$(jq -c \
    --arg name "$name" \
    --arg id "$id" \
    '.[$name] = $id' <<<"$node_definition_ids")

  echo "seed-s3fetch: created node definition $name ($id, schema: $schema_type)" >&2
done < <(jq -er 'keys[]' <<<"$node_definitions")

jq -n \
  --argjson node_definition_ids "$node_definition_ids" \
  '{node_definition_ids: $node_definition_ids}'

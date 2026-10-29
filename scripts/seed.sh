#!/usr/bin/env bash
# Create the sample forms, then sample node definitions, then the sample
# workflows with the returned IDs: "main" (private input node),
# "main-public" (same graph, public input node), and "main-upload"
# (same graph, multipart file-upload input node backed by infra.storage).
#
# Usage: scripts/seed.sh [BASE_URL]

set -euo pipefail

BASE_URL="${1:-http://localhost:8080}"

# Auto-resolve the API token when not exported: the compose stack is the dev
# default source of truth, with config.yaml as fallback for non-compose runs.
# An exported BEELINEWF_API_TOKEN always wins.
if [[ -z "${BEELINEWF_API_TOKEN:-}" ]]; then
  _seed_root="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." && pwd)"
  _token_candidate="$(grep -m1 -E 'BEELINEWF_API_TOKEN:' "$_seed_root/docker-compose.yml" 2>/dev/null | sed -E 's/^[^:]*:[[:space:]]*"?([^"#]+)"?.*/\1/' | tr -d '[:space:]"' | tr -d "'" || true)"
  # Unwrap compose ${VAR:-default} interpolation to the default.
  if [[ "${_token_candidate:-}" == \$\{* ]]; then
    _token_candidate="${_token_candidate##*:-}"
    _token_candidate="${_token_candidate%\}}"
  fi
  if [[ -z "${_token_candidate:-}" ]]; then
    _token_candidate="$(grep -m1 -E '^[[:space:]]*api_token:' "$_seed_root/config.yaml" 2>/dev/null | sed -E 's/^[^:]*:[[:space:]]*"?([^"#]+)"?.*/\1/' | tr -d '[:space:]"' | tr -d "'" || true)"
  fi
  if [[ -n "${_token_candidate:-}" ]]; then
    export BEELINEWF_API_TOKEN="$_token_candidate"
    echo "seed: using auto-resolved API token" >&2
  fi
  unset _seed_root _token_candidate
fi

fail() {
  echo "seed: FAIL: $*" >&2
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

# The private and public post inputs render the same post form, while the
# file-upload input renders its own upload form matching its files+fields
# validation contract and PDF storage constraints. The definition create
# calls below reference the returned ids.
form_payload=$(cat <<'JSON'
{
  "name": "post-form",
  "schema": {
    "type": "object",
    "required": ["title", "body"],
    "properties": {
      "title": {"type": "string", "minLength": 1},
      "body": {"type": "string", "minLength": 1}
    }
  },
  "ui": {
    "order": ["title", "body"],
    "fields": {
      "title": {"label": "Title", "placeholder": "Post title"},
      "body": {"label": "Body", "widget": "textarea", "placeholder": "Write something..."}
    }
  }
}
JSON
)

form_response=$(api_curl -fsS -X POST \
  -H 'Content-Type: application/json' \
  --data-binary "$form_payload" \
  "$BASE_URL/v1/forms") \
  || fail "create post form"

form_id=$(jq -er '.id' <<<"$form_response") \
  || fail "parse post form id"

echo "seed: created form post-form ($form_id)" >&2

upload_form_payload=$(cat <<'JSON'
{
  "name": "upload-form",
  "content_type": "multipart/form-data",
  "schema": {
    "type": "object",
    "required": ["files", "fields"],
    "properties": {
      "files": {
        "type": "array",
        "minItems": 1,
        "items": {"type": "string", "format": "binary"},
        "allowed_mime_types": ["application/pdf"],
        "max_file_bytes": 1048576
      },
      "fields": {
        "type": "array",
        "minItems": 1,
        "items": {"type": "string", "minLength": 1}
      }
    }
  },
  "ui": {
    "order": ["files", "fields"],
    "fields": {
      "files": {"label": "Files", "widget": "file", "accept": "application/pdf"},
      "fields": {"label": "Fields", "placeholder": "Field value"}
    }
  }
}
JSON
)

upload_form_response=$(api_curl -fsS -X POST \
  -H 'Content-Type: application/json' \
  --data-binary "$upload_form_payload" \
  "$BASE_URL/v1/forms") \
  || fail "create upload form"

upload_form_id=$(jq -er '.id' <<<"$upload_form_response") \
  || fail "parse upload form id"

echo "seed: created form upload-form ($upload_form_id)" >&2

node_definitions=$(cat <<'JSON'
{
  "script": {
    "name": "calculate-total",
    "type": "script",
    "content": {
      "type": "script",
      "script": "return input.length;",
      "input_data": "posts.Body",
      "output_property": "total",
      "timeout": "30s"
    }
  },
  "script_no_output": {
    "name": "add-lucky-number",
    "type": "script",
    "content": {
      "type": "script",
      "script": "return Math.floor(Math.random() * 1000);",
      "timeout": "30s"
    }
  },
  "conditions": {
    "name": "route-by-total",
    "type": "conditions",
    "content": {
      "type": "conditions",
      "conditions": [
        {
          "condition": "return context.posts.Body.length >= 100;",
          "key": "many"
        },
        {
          "condition": "return context.posts.Body.length < 100;",
          "key": "less"
        }
      ]
    }
  },
  "input": {
    "name": "new-post",
    "type": "input",
    "content": {
      "type": "input",
      "channel": "http",
      "output_property": "new_post",
      "validation": {
        "script": "return input != null && Object.prototype.toString.call(input) === '[object Object]';"
      }
    }
  },
  "input_public": {
    "name": "public-post",
    "type": "input",
    "content": {
      "type": "input",
      "channel": "http",
      "output_property": "public_post",
      "validation": {
        "script": "return input != null && Object.prototype.toString.call(input) === '[object Object]';"
      },
      "public": true
    }
  },
  "input_upload": {
    "name": "upload-post",
    "type": "input",
    "content": {
      "type": "input",
      "channel": "http",
      "output_property": "upload_post",
      "accept": ["application/json", "multipart/form-data"],
      "storage": {
        "bucket": "uploads",
        "prefix": "posts/",
        "allowed_mime_types": ["application/pdf"],
        "max_file_bytes": 1048576
      },
      "validation": {
        "script": "input = JSON.parse(input); if (!Array.isArray(input.files) || input.files.length < 1) { return 'files must contain at least 1 item'; } if (!Array.isArray(input.fields) || input.fields.length < 1) { return 'fields must contain at least 1 item'; }"
      }
    }
  },
  "external_call": {
    "name": "get-posts",
    "type": "external_call",
    "content": {
      "type": "external_call",
      "http_config": {
        "url": "https://jsonplaceholder.typicode.com/posts",
        "method": "GET"
      },
      "output_property": "posts",
      "timeout": "30s",
      "retry_on_recovery": true
    }
  }
}
JSON
)

# The "less" key has no target, so that branch ends the run: main parks on
# the private new-post input only when there are many posts.
workflow_template=$(cat <<'JSON'
{
  "content": {
    "start_node_id": "019fea41-0001-7000-8000-000000000001",
    "status_update": {
      "redis": {}
    },
    "keys": {
      "many": "019fea41-3005-758d-a647-ad9a0ca8e21a",
      "less": null
    },
    "nodes": [
      {
        "id": "019fea41-0001-7000-8000-000000000001",
        "name": "get-posts",
        "output_property": "posts",
        "retry_on_recovery": true,
        "next_node": "019fea41-0682-7c77-9eec-ae9037cc0e71"
      },
      {
        "id": "019fea41-0682-7c77-9eec-ae9037cc0e71",
        "name": "route-by-total"
      },
      {
        "id": "019fea41-3005-758d-a647-ad9a0ca8e21a",
        "name": "new-post",
        "next_node": "019fea40-ddb2-7b4b-98fb-1c01a04d33e8"
      },
      {
        "id": "019fea40-ddb2-7b4b-98fb-1c01a04d33e8",
        "name": "calculate-total",
        "output_property": "total",
        "next_node": "019fea42-0000-7016-8814-16cbe5663eff"
      },
      {
        "id": "019fea42-0000-7016-8814-16cbe5663eff",
        "name": "add-lucky-number"
      }
    ]
  },
  "name": "main"
}
JSON
)

# The same graph as main, except the "many" branch parks on a public input
# node instead of the private one, so it accepts an anonymous delivery with no
# credential at all. The "less" branch ends the run, as in main. Node ids are
# distinct from main so both definitions coexist.
workflow_public_template=$(cat <<'JSON'
{
  "content": {
    "start_node_id": "01a0fcdb-cbfd-7f95-b6a5-e836c523cd58",
    "status_update": {
      "redis": {}
    },
    "keys": {
      "many": "01a0fcdb-cbfd-756b-820b-b0b6e5d6c853",
      "less": null
    },
    "nodes": [
      {
        "id": "01a0fcdb-cbfd-7f95-b6a5-e836c523cd58",
        "name": "get-posts",
        "output_property": "posts",
        "retry_on_recovery": true,
        "next_node": "01a0fcdb-cbfd-79b1-ad03-108fc6663e1d"
      },
      {
        "id": "01a0fcdb-cbfd-79b1-ad03-108fc6663e1d",
        "name": "route-by-total"
      },
      {
        "id": "01a0fcdb-cbfd-756b-820b-b0b6e5d6c853",
        "name": "public-post",
        "next_node": "01a0fcdb-cbfd-7060-9acc-b5617f757925"
      },
      {
        "id": "01a0fcdb-cbfd-7060-9acc-b5617f757925",
        "name": "calculate-total",
        "output_property": "total",
        "next_node": "01a0fcdb-cbfd-7d5d-a8f1-c32d66004f8f"
      },
      {
        "id": "01a0fcdb-cbfd-7d5d-a8f1-c32d66004f8f",
        "name": "add-lucky-number"
      }
    ]
  },
  "name": "main-public"
}
JSON
)

# The same graph as main, except the "many" branch parks on the
# multipart file-upload input node: deliveries may be JSON or
# multipart/form-data, and file parts stream into the "uploads" bucket
# via infra.storage. Node ids are distinct from main and main-public so
# all three definitions coexist.
workflow_upload_template=$(cat <<'JSON'
{
  "content": {
    "start_node_id": "01a1fcdb-cbfd-7f95-b6a5-e836c523cd58",
    "status_update": {
      "redis": {}
    },
    "keys": {
      "many": "01a1fcdb-cbfd-756b-820b-b0b6e5d6c853",
      "less": null
    },
    "nodes": [
      {
        "id": "01a1fcdb-cbfd-7f95-b6a5-e836c523cd58",
        "name": "get-posts",
        "output_property": "posts",
        "retry_on_recovery": true,
        "next_node": "01a1fcdb-cbfd-79b1-ad03-108fc6663e1d"
      },
      {
        "id": "01a1fcdb-cbfd-79b1-ad03-108fc6663e1d",
        "name": "route-by-total"
      },
      {
        "id": "01a1fcdb-cbfd-756b-820b-b0b6e5d6c853",
        "name": "upload-post",
        "next_node": "01a1fcdb-cbfd-7060-9acc-b5617f757925"
      },
      {
        "id": "01a1fcdb-cbfd-7060-9acc-b5617f757925",
        "name": "calculate-total",
        "output_property": "total",
        "next_node": "01a1fcdb-cbfd-7d5d-a8f1-c32d66004f8f"
      },
      {
        "id": "01a1fcdb-cbfd-7d5d-a8f1-c32d66004f8f",
        "name": "add-lucky-number"
      }
    ]
  },
  "name": "main-upload"
}
JSON
)

# Point the post inputs at the post form and the upload input at the
# upload form created above.
node_definitions=$(jq -c --arg form_id "$form_id" --arg upload_form_id "$upload_form_id" '
  .input.content.form_id = $form_id
  | .input_public.content.form_id = $form_id
  | .input_upload.content.form_id = $upload_form_id
' <<<"$node_definitions") || fail "attach form_id to input definitions"

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

  echo "seed: created node definition $name ($id, schema: $schema_type)" >&2
done < <(jq -er 'keys[]' <<<"$node_definitions")

# create_workflow resolves node definitions, posts the definition, and prints
# the created id on stdout. Every node definition is created above, so the
# same id map serves all workflow templates.
create_workflow() {
  local template=$1
  local label=$2
  local payload response id schemas

  payload=$(jq -ce --argjson ids "$node_definition_ids" '
    .content.nodes |= map(
      .node_definition_id = (
        $ids[.name] // error("no node definition id for workflow node " + .name)
      )
    )
  ' <<<"$template") || fail "build $label workflow definition payload"

  response=$(api_curl -fsS -X POST \
    -H 'Content-Type: application/json' \
    --data-binary "$payload" \
    "$BASE_URL/v1/workflow/definition") \
    || fail "create $label workflow definition"

  id=$(jq -er '.id' <<<"$response") \
    || fail "parse $label workflow definition id"
  schemas=$(jq -er '.schemas | keys | sort | join(",")' <<<"$response") \
    || fail "$label workflow definition response missing schemas"

  echo "seed: created $label workflow definition $id (schemas: $schemas)" >&2
  printf '%s\n' "$id"
}

main_workflow_definition_id=$(create_workflow "$workflow_template" main) \
  || fail "create main workflow definition"
public_workflow_definition_id=$(create_workflow "$workflow_public_template" main-public) \
  || fail "create public-input workflow definition"
upload_workflow_definition_id=$(create_workflow "$workflow_upload_template" main-upload) \
  || fail "create upload-input workflow definition"

jq -n \
  --arg form_id "$form_id" \
  --arg upload_form_id "$upload_form_id" \
  --arg workflow_definition_id "$main_workflow_definition_id" \
  --arg public_workflow_definition_id "$public_workflow_definition_id" \
  --arg upload_workflow_definition_id "$upload_workflow_definition_id" \
  --argjson node_definition_ids "$node_definition_ids" \
  '{
    form_id: $form_id,
    upload_form_id: $upload_form_id,
    node_definition_ids: $node_definition_ids,
    workflow_definition_id: $workflow_definition_id,
    public_workflow_definition_id: $public_workflow_definition_id,
    upload_workflow_definition_id: $upload_workflow_definition_id
  }'

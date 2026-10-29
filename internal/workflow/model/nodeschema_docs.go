package model

// The per-type schema documents below are authored by hand, one per builtin
// node type. They mirror the Go parser in nodecontent.go: required fields,
// enums, and nested objects follow the parser rules, and nothing else is
// accepted. They are documentation only — the parser stays authoritative —
// and are served verbatim on node and workflow definition reads so a
// generic frontend can render a form per type.
//
// Every document describes the inline workflow-occurrence shape. A node
// referencing node_definition_id carries only graph fields instead; that
// reference shape is documented once in docs/integration/fields.md.

// commonNodeSchemaFields is the JSON fragment of the graph and lifecycle
// fields every node type accepts. It is a comma-separated property list
// pasted into the "properties" object of each hand-authored document below;
// the parser decides which types additionally allow on_failure.
const commonNodeSchemaFields = `
    "id": { "$ref": "#/$defs/nodeId" },
    "name": { "type": "string" },
    "retry_on_recovery": { "type": "boolean" },
    "metadata": { "type": "object", "description": "Opaque metadata; never read by the engine." },
    "pre_script": { "$ref": "#/$defs/hook" },
    "post_script": { "$ref": "#/$defs/hook" }`

const scriptNodeSchemaJSON = `{
  "title": "beelinewf script node",
  "description": "Runs a script against the workflow context. Go parsers stay authoritative.",
  "type": "object",
  "properties": {
    "type": { "const": "script" },
    "script": { "type": "string", "minLength": 1, "description": "Non-blank script; its return value is stored at output_property." },
    "input_data": { "type": "string", "description": "Context path selecting the value exposed to the script as input." },
    "timeout": { "$ref": "#/$defs/duration" },
    "output_property": { "$ref": "#/$defs/contextKey" },
    "next_node": { "$ref": "#/$defs/nodeId" },
    ` + commonNodeSchemaFields + `
  },
  "required": ["type", "script"],
  "additionalProperties": false
}`

const conditionsNodeSchemaJSON = `{
  "title": "beelinewf conditions node",
  "description": "Routes execution to a workflow or group key. Carries neither next_node nor output_property. Go parsers stay authoritative.",
  "type": "object",
  "properties": {
    "type": { "const": "conditions" },
    "conditions": {
      "type": "array",
      "minItems": 2,
      "description": "Each script must return a boolean. A blank key exits the scope; a non-blank key must be defined in the workflow or group keys.",
      "items": {
        "type": "object",
        "properties": {
          "condition": { "type": "string", "minLength": 1 },
          "key": { "type": "string" }
        },
        "required": ["condition"],
        "additionalProperties": false
      }
    },
    "id": { "$ref": "#/$defs/nodeId" },
    "name": { "type": "string" },
    "metadata": { "type": "object", "description": "Opaque metadata; never read by the engine." },
    "pre_script": { "$ref": "#/$defs/hook" },
    "post_script": { "$ref": "#/$defs/hook" }
  },
  "required": ["type", "conditions"],
  "additionalProperties": false
}`

const inputNodeSchemaJSON = `{
  "title": "beelinewf input node",
  "description": "Parks the instance until a payload arrives on a channel. Go parsers stay authoritative.",
  "type": "object",
  "properties": {
    "type": { "const": "input" },
    "channel": { "type": "string", "enum": ["http", "redis", "rabbitmq"] },
    "output_property": { "type": "string", "pattern": "^[A-Za-z0-9_]+$", "description": "Bare key receiving the accepted payload; blank means the graph node id." },
    "validation": {
      "type": "object",
      "description": "Script rejecting non-blank string returns, evaluated with the raw payload as input.",
      "properties": { "script": { "type": "string", "minLength": 1 } },
      "required": ["script"],
      "additionalProperties": false
    },
    "form_id": { "$ref": "#/$defs/nodeId", "description": "Id of the form in the forms library the frontend renders for this node. The engine never reads forms." },
    "allowed_roles": {
      "type": "array",
      "description": "Roles allowed to deliver to this input node, checked against the caller's live token roles after the endpoint permission. Absent or empty is open to any caller. The two gates are a union: a listed role passes even when allowed_users is non-empty. Ignored on a public node.",
      "items": { "type": "string", "minLength": 1 }
    },
    "allowed_users": {
      "type": "array",
      "description": "Users allowed to deliver to this input node, checked against the caller's resolved user id. A listed user passes however their roles read. The two gates are a union: a listed role passes even when this list is non-empty. Absent or empty means no user restriction. Ignored on a public node.",
      "items": {
        "type": "object",
        "properties": { "id": { "$ref": "#/$defs/nodeId", "description": "Users.id uuid of an allowed caller." } },
        "required": ["id"],
        "additionalProperties": false
      }
    },
    "record_actor": {
      "type": "boolean",
      "description": "Write {user_id, input_data} under output_property instead of the bare payload, so templates read {{ key.input_data.x }}. The service principal records the system user."
    },
    "public": {
      "type": "boolean",
      "description": "Anonymous callers may deliver to this node over HTTP with no credential, even when auth is enabled. Ignores allowed_roles and allowed_users while keeping them stored; still forces record_actor=false; anonymous deliveries are marked in history."
    },
    "accept": {
      "type": "array",
      "description": "Request media types the node delivers. Absent means application/json only. multipart/form-data streams file parts into object storage and needs channel http plus storage.",
      "items": { "type": "string", "enum": ["application/json", "multipart/form-data"] }
    },
    "storage": {
      "type": "object",
      "description": "Multipart upload target: bucket, optional key prefix, and optional file constraints. Required exactly when accept lists multipart/form-data. The endpoint and credentials live in server config, never here.",
      "properties": {
        "bucket": { "type": "string", "minLength": 1, "description": "Destination bucket; no slashes or whitespace." },
        "prefix": { "type": "string", "description": "Prepended to every object key; a .. segment is rejected." },
        "allowed_mime_types": {
          "type": "array",
          "description": "File allowlist, each type/subtype or type/*. Empty means unconstrained beyond engine limits.",
          "items": { "type": "string", "minLength": 1 }
        },
        "max_file_bytes": { "type": "integer", "minimum": 1, "description": "Per-file byte cap; absent means the engine cap." }
      },
      "required": ["bucket"],
      "additionalProperties": false
    },
    "next_node": { "$ref": "#/$defs/nodeId" },
    "retry_on_recovery": { "type": "boolean" },
    "metadata": { "type": "object", "description": "Opaque metadata; never read by the engine." },
    "pre_script": { "$ref": "#/$defs/hook" },
    "post_script": { "$ref": "#/$defs/hook" }
  },
  "required": ["type", "channel"],
  "additionalProperties": false
}`

const groupNodeSchemaJSON = `{
  "title": "beelinewf group node",
  "description": "Runs a nested node graph starting at start_node_id. Nested children follow the same per-type rules; go parsers stay authoritative.",
  "type": "object",
  "properties": {
    "type": { "const": "group" },
    "start_node_id": { "$ref": "#/$defs/nodeId" },
    "keys": { "$ref": "#/$defs/keys" },
    "nodes": {
      "type": "array",
      "minItems": 1,
      "description": "Nested child nodes with uuids unique across the whole definition.",
      "items": { "type": "object" }
    },
    "output_property": { "$ref": "#/$defs/contextKey" },
    "next_node": { "$ref": "#/$defs/nodeId" },
    "metadata": { "type": "object", "description": "Opaque metadata; never read by the engine." },
    "pre_script": { "$ref": "#/$defs/hook" },
    "post_script": { "$ref": "#/$defs/hook" }
  },
  "required": ["type", "start_node_id", "nodes"],
  "additionalProperties": false
}`

const externalCallNodeSchemaJSON = `{
  "title": "beelinewf external_call node",
  "description": "Makes exactly one outbound HTTP call, executes an allowlisted command, or sends one templated email. Go parsers stay authoritative.",
  "type": "object",
  "properties": {
    "type": { "const": "external_call" },
    "http_config": {
      "type": "object",
      "description": "Outbound HTTP call. Templated values validate at execution time after rendering.",
      "properties": {
        "url": { "type": "string", "minLength": 1, "description": "Absolute http(s) url, or a {{ path }} template." },
        "method": { "type": "string", "description": "Blank defaults to GET; templated values resolve at execution time." },
        "headers": { "type": "object", "additionalProperties": { "type": "string" } },
        "body": {}
      },
      "required": ["url"],
      "additionalProperties": false
    },
    "execution_config": {
      "type": "object",
      "description": "Allowlisted command execution. argv is literal and never passed through a shell. command[0] must be an absolute path in engine.exec_allowlist.",
      "properties": {
        "command": { "type": "array", "minItems": 1, "items": { "type": "string", "minLength": 1 } },
        "stdin": { "type": "string" }
      },
      "required": ["command"],
      "additionalProperties": false
    },
    "smtp_config": {
      "type": "object",
      "description": "Templated email sent through the operator relay (engine.smtp). Every string field renders {{ path }} placeholders against workflow context. SMTP has no idempotency: a recovery requeue or retry_on_recovery can deliver twice; a deterministic Message-ID aids downstream dedup.",
      "properties": {
        "to": { "type": "array", "minItems": 1, "items": { "type": "string", "minLength": 1 }, "description": "Recipients. A lone-placeholder element expands a typed string list from context." },
        "cc": { "type": "array", "items": { "type": "string" } },
        "bcc": { "type": "array", "items": { "type": "string" } },
        "from": { "type": "string", "description": "Sender override; blank defaults to the relay from." },
        "reply_to": { "type": "string" },
        "subject": { "type": "string", "description": "Must render to a single line." },
        "text": { "type": "string" },
        "html": { "type": "string", "description": "Inline HTML part; mutually exclusive with file." },
        "file": {
          "type": "object",
          "description": "Alternative HTML source to inline html, mutually exclusive with it. The rendered path must end .html/.htm; s3://bucket/key reads via global storage, any other relative path reads under engine.smtp.template_dir. Contents render {{ path }} placeholders against data alone, never the full workflow context.",
          "properties": {
            "path": { "type": "string", "minLength": 1 },
            "data": { "type": "object", "description": "Keys must be bare context keys; every string leaf renders against workflow context first." }
          },
          "required": ["path"],
          "additionalProperties": false
        }
      },
      "required": ["to"],
      "additionalProperties": false
    },
    "timeout": { "$ref": "#/$defs/duration" },
    "output_property": { "$ref": "#/$defs/contextKey" },
    "next_node": { "$ref": "#/$defs/nodeId" },
    "on_failure": { "$ref": "#/$defs/failureRoute" },
    ` + commonNodeSchemaFields + `
  },
  "required": ["type"],
  "oneOf": [{ "required": ["http_config"] }, { "required": ["execution_config"] }, { "required": ["smtp_config"] }],
  "additionalProperties": false
}`

const outputNodeSchemaJSON = `{
  "title": "beelinewf output node",
  "description": "Publishes the exact JSON at context_path to a broker channel. Go parsers stay authoritative.",
  "type": "object",
  "properties": {
    "type": { "const": "output" },
    "channel": { "type": "string", "enum": ["redis", "rabbitmq"] },
    "context_path": { "type": "string", "minLength": 1, "description": "Context path whose value is published." },
    "timeout": { "$ref": "#/$defs/duration" },
    "next_node": { "$ref": "#/$defs/nodeId" },
    "output_property": { "$ref": "#/$defs/contextKey" },
    "metadata": { "type": "object", "description": "Opaque metadata; never read by the engine." },
    "pre_script": { "$ref": "#/$defs/hook" },
    "post_script": { "$ref": "#/$defs/hook" }
  },
  "required": ["type", "channel", "context_path"],
  "additionalProperties": false
}`

const pollerNodeSchemaJSON = `{
  "title": "beelinewf poller node",
  "description": "Repeatedly calls a transport or waits on one until the until predicate returns true. Exactly one transport block is required. Go parsers stay authoritative.",
  "type": "object",
  "properties": {
    "type": { "const": "poller" },
    "http": {
      "type": "object",
      "description": "Repeated HTTP calls. Defaults: GET / 5s delay / 30s request_timeout / 10 attempts.",
      "properties": {
        "url": { "type": "string", "minLength": 1, "description": "Absolute http(s) url, or a {{ path }} template." },
        "method": { "type": "string" },
        "headers": { "type": "object", "additionalProperties": { "type": "string" } },
        "body": {},
        "delay": { "$ref": "#/$defs/duration" },
        "request_timeout": { "$ref": "#/$defs/duration" },
        "max_attempts": { "type": "integer", "minimum": 1, "default": 10 },
        "until": { "type": "string", "minLength": 1, "description": "Predicate evaluated against the normalized response; must return a boolean." }
      },
      "required": ["url", "until"],
      "additionalProperties": false
    },
    "redis": {
      "type": "object",
      "description": "GET requires key; SUB requires channel and forbids key, delay, request_timeout, and max_attempts. MaxWaitTime default 5m.",
      "properties": {
        "method": { "type": "string", "enum": ["GET", "SUB"] },
        "key": { "type": "string" },
        "channel": { "type": "string" },
        "delay": { "$ref": "#/$defs/duration" },
        "request_timeout": { "$ref": "#/$defs/duration" },
        "max_attempts": { "type": "integer", "minimum": 1, "default": 10 },
        "max_wait_time": { "$ref": "#/$defs/duration" },
        "until": { "type": "string", "minLength": 1, "description": "Predicate evaluated against the normalized response; must return a boolean." }
      },
      "required": ["until"],
      "additionalProperties": false
    },
    "rabbitmq": {
      "type": "object",
      "description": "Waits for the first message on a pre-provisioned exclusive queue. MaxWaitTime default 5m.",
      "properties": {
        "queue": { "type": "string", "minLength": 1 },
        "max_wait_time": { "$ref": "#/$defs/duration" },
        "until": { "type": "string", "minLength": 1, "description": "Predicate evaluated against the normalized response; must return a boolean." }
      },
      "required": ["queue", "until"],
      "additionalProperties": false
    },
    "timeout": { "$ref": "#/$defs/duration" },
    "on_failure": { "$ref": "#/$defs/failureRoute" },
    "retry_on_recovery": { "type": "boolean", "default": true, "description": "Pollers are active waits and requeue by default when the worker lease expires." },
    "id": { "$ref": "#/$defs/nodeId" },
    "name": { "type": "string" },
    "output_property": { "$ref": "#/$defs/contextKey" },
    "next_node": { "$ref": "#/$defs/nodeId" },
    "metadata": { "type": "object", "description": "Opaque metadata; never read by the engine." },
    "pre_script": { "$ref": "#/$defs/hook" },
    "post_script": { "$ref": "#/$defs/hook" }
  },
  "required": ["type"],
  "oneOf": [{ "required": ["http"] }, { "required": ["redis"] }, { "required": ["rabbitmq"] }],
  "additionalProperties": false
}`

const parallelStartNodeSchemaJSON = `{
  "title": "beelinewf parallel_start node",
  "description": "Forks execution into named branches that each run with an isolated context; the parent resumes at parallel_end_node_id after every branch reaches the join. Go parsers stay authoritative.",
  "type": "object",
  "properties": {
    "type": { "const": "parallel_start" },
    "branches": {
      "type": "object",
      "minProperties": 2,
      "description": "Branch name to same-scope target node id; at least two branches are required.",
      "additionalProperties": { "$ref": "#/$defs/nodeId" }
    },
    "parallel_end_node_id": { "$ref": "#/$defs/nodeId" },
    ` + commonNodeSchemaFields + `
  },
  "required": ["type", "branches", "parallel_end_node_id"],
  "additionalProperties": false
}`

const parallelEndNodeSchemaJSON = `{
  "title": "beelinewf parallel_end node",
  "description": "Barrier join: runs once after every branch of the matching parallel_start arrives, merging branch contexts into the parent via combining_script. Go parsers stay authoritative.",
  "type": "object",
  "properties": {
    "type": { "const": "parallel_end" },
    "combining_script": { "type": "string", "minLength": 1, "description": "Non-blank script; reads branch contexts through the read-only branch global and writes the parent context." },
    "next_node": { "$ref": "#/$defs/nodeId" },
    ` + commonNodeSchemaFields + `
  },
  "required": ["type", "combining_script"],
  "additionalProperties": false
}`

const delayNodeSchemaJSON = `{
  "title": "beelinewf delay node",
  "description": "Waits the configured duration on arrival, then completes. The explicit form of start_delay, which delay nodes do not accept. Go parsers stay authoritative.",
  "type": "object",
  "properties": {
    "type": { "const": "delay" },
    "delay": { "$ref": "#/$defs/duration" },
    "input_data": { "type": "string", "description": "Context path selecting the value exposed to the node as input." },
    "output_property": { "$ref": "#/$defs/contextKey" },
    "next_node": { "$ref": "#/$defs/nodeId" },
    "retry_on_recovery": { "type": "boolean", "default": true, "description": "Delay waits are idempotent and requeue by default when the worker lease expires." },
    "id": { "$ref": "#/$defs/nodeId" },
    "name": { "type": "string" },
    "metadata": { "type": "object", "description": "Opaque metadata; never read by the engine." },
    "pre_script": { "$ref": "#/$defs/hook" },
    "post_script": { "$ref": "#/$defs/hook" }
  },
  "required": ["type", "delay"],
  "additionalProperties": false
}`

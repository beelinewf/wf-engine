<img src="./banner.png" />

# BeelineWF — Durable Workflow Engine on PostgreSQL

[![CI](https://github.com/beelinewf/wf-engine/actions/workflows/ci.yml/badge.svg)](https://github.com/beelinewf/wf-engine/actions/workflows/ci.yml)
![Coverage](https://img.shields.io/badge/Coverage-80.3%25-brightgreen)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

BeelineWF is a durable workflow engine in Go. Workflow definitions are
immutable and versioned, execution state lives in PostgreSQL, and workers
claim work with `SELECT ... FOR UPDATE SKIP LOCKED` under leases and
heartbeats. An HTTP API covers definitions, instances, input delivery,
pause/resume/stop/rollback controls, and per-node debugging. Optional Redis
and RabbitMQ transports add broker input nodes, broker output nodes, and
status notifications. Both stay fully disabled when their DSN is absent.

## Contents

- [Why BeelineWF](#why-beelinewf)
- [Features](#features)
- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Quickstart](#quickstart)
- [Your first workflow](#your-first-workflow)
- [Node types](#node-types)
- [Configuration](#configuration)
- [API reference](#api-reference)
- [Status notifications](#status-notifications)
- [Development](#development)
- [Project layout](#project-layout)
- [Further reading](#further-reading)
- [Contributing](#contributing)
- [Security](#security)
- [License](#license)

## Why BeelineWF

- **Durable by default.** Every state transition is checkpointed in
  PostgreSQL with lease plus revision fencing, so crashed workers never
  corrupt a run. Recovery rules per node type decide between retry and fail.
- **Definitions are immutable.** New versions link to the previous one
  (`previous_version_id`, `lineage_id`). Running instances always execute
  the exact definition they started with.
- **Postgres first, brokers optional.** The engine needs only PostgreSQL.
  Redis pub/sub and RabbitMQ durable queues plug in for event-driven input,
  output publishing, and status fan-out, and the app still starts
  HTTP-only when both DSNs are empty.
- **Debuggable.** Full `context_before`/`context_after` snapshots per
  attempt, an append-only event log, per-occurrence node debug, and
  rollback of paused or failed instances to a prior occurrence.

## Features

- Immutable node and workflow definitions with linear versioning and graph
  validation.
- Secret management through `/v1/secrets`: create, list, get, update, and delete
  key-value pairs. Read responses contain only a constant `********` mask.
  Instances freeze the current secret values under `secret` at create time,
  so workflows can render `{{ secret.KEY }}` while later secret changes do
  not affect running instances.
- Node types: `script` (Goja ES5.1 sandbox, no `eval`), `conditions`
  (exactly-one-match routing), `input` (HTTP webhook, Redis pub/sub, or
  RabbitMQ queue with optional `form_id` reference into the `/v1/forms` library
  — JSON Schema plus ui hints — plus validation script and `Idempotency-Key` dedupe;
  instances created with `"snapshot_forms": true` freeze those bytes at start),
  `output` (publish a context value to Redis or RabbitMQ, returns a
  receipt), nested `group`, `external_call` (outbound HTTP or allowlisted
  command), and `poller` (active wait over HTTP, Redis, or RabbitMQ until
  an `until` predicate matches), `delay` (explicit timed wait, the node
  form of `start_delay`), plus `parallel_start`/`parallel_end` for
  fork/join branches with snapshot isolation and a scripted merge.
- Lifecycle hooks: optional `pre_script`/`post_script` context transforms
  on every node type, run in the same sandbox.
- Failure routing: `external_call` and `poller` nodes can route failures
  to a fallback node via `on_failure` instead of failing the instance.
- Controls: `pause` (immediate, or deferred after the current node),
  `resume`, terminal `stop` (fences workers, cancels in-flight execution),
  and `rollback` to a prior node occurrence (always lands paused).
- Status notifications: per-definition `status_update` webhooks and broker
  messages, delivered from a PostgreSQL transactional outbox, ordered per
  instance and per transport, at-least-once with shared idempotency keys.
- Auth: two independent credentials on `/v1` (`/health/*` stays public, an
  input node with `public: true` accepts an anonymous delivery, and the
  single form and workflow-definition reads are public for it).
  An OIDC bearer token authenticates a human; the engine is a stateless
  resource server with no callback, so the frontend runs code+PKCE against
  the provider and calls the API with the JWT. An `X-Api-Token` authenticates
  the service principal, which bypasses every authorization gate and is
  recorded as the system user. With neither configured, `/v1` is open as
  before. Authorization is role-based over a database-backed action catalog
  (see [Authentication and authorization](#authentication-and-authorization)).
  Swagger UI at `/swagger/index.html`.

## How it works

```mermaid
flowchart LR
    API[HTTP API] --> DB[(PostgreSQL)]
    DISP[Dispatch loop] -->|ClaimNext FOR UPDATE SKIP LOCKED| DB
    DISP --> ENG[Engine: one node transition per claim]
    ENG --> EX[Executors: script / conditions / input / output / HTTP / command / poller]
    ENG -->|fenced checkpoint + outbox| DB
    HB[Heartbeat] -->|renew leases, poll stops| DB
```

1. A dispatcher claims a runnable instance and leases it to a worker.
2. The engine runs exactly one node transition, then checkpoints the new
   cursor (status `waiting`, `paused`, `finished`, or `failed`).
3. Checkpoints are fenced writes (`WHERE id AND revision AND leased_by`).
   A stale worker loses and aborts silently; a stop always wins.
4. Expired leases trigger recovery: scripts requeue, input nodes re-park,
   other node types retry only with `retry_on_recovery=true` (pollers
   default it to true).
5. Status changes enqueue outbox rows in the same transaction; a dedicated
   dispatcher delivers them per transport, in order, with retries.

See [ARCHITECTURE.md](ARCHITECTURE.md) for package boundaries, the full
state machines, and recovery semantics.

## Requirements

- Go 1.26+
- Docker and Docker Compose (fastest path, or a local PostgreSQL 16)
- [Atlas CLI](https://atlasgo.io/) (schema migrations; the app never migrates)
- `curl` and `jq` (for `scripts/seed.sh`, `scripts/e2e.sh`, and
  `scripts/e2e-oidc.sh`)
- For `task e2e-oidc` only: `python3` with the `cryptography` package, and
  the `openssl` CLI. The throwaway mock provider signs real RS256 JWTs
  rather than serving unsigned ones, so the app verifies the signature
  exactly as it would against Zitadel or Keycloak. Install with
  `pip install cryptography`.
- Optional: Redis 7, RabbitMQ 4 (only for broker transports)

## Quickstart

### Option A: Docker Compose (fastest)

Starts PostgreSQL, Redis, RabbitMQ, migrations, and the app with all
transports enabled. The stack is API-only: there is no bundled web UI, so
deploy a frontend separately against `http://localhost:8080`. CORS allows
any origin by default; set `BEELINEWF_INFRA_HTTP_CORS_ALLOWED_ORIGINS` to
restrict it.

```bash
docker compose up --build
curl http://localhost:8080/health/ready
```

- App: `http://localhost:8080` (PostgreSQL `9921`, Redis `6379`,
  RabbitMQ `5672`, management UI `http://localhost:15672` with
  `beelinewf` / `beelinewf`).
- Swagger UI: `http://localhost:8080/swagger/index.html`.
- Auth is disabled in the compose defaults. To require the service credential,
  set `BEELINEWF_AUTH_ENABLED=true` and pass `X-Api-Token: wadidaw` on `/v1` calls.
  To accept human tokens too, set `BEELINEWF_AUTH_OIDC_ISSUER` and
  `BEELINEWF_AUTH_OIDC_CLIENT_ID` — or use the bundled `mock-oidc` service for a
  zero-setup provider (test only: it mints a JWT for anyone who asks):
  `BEELINEWF_AUTH_OIDC_ENABLED=true BEELINEWF_AUTH_OIDC_ISSUER=http://mock-oidc:9099
  BEELINEWF_AUTH_OIDC_CLIENT_ID=beelinewf docker compose up --build`, then
  `curl http://localhost:9099/token/admin` (`auditor`/`finance` cover the other
  shipped roles). The stack's role catalog is seeded from the compose
  file's `BEELINEWF_AUTH_ROLE_PERMISSIONS` on first boot.
- Broker-free mode: unset `BEELINEWF_INFRA_REDIS_DSN` and
  `BEELINEWF_INFRA_RABBITMQ_DSN` on the `app` service.

### Option B: Local Go

```bash
# PostgreSQL (matches config.yaml)
docker run -d --name beelinewf-pg -p 9921:5432 \
  -e POSTGRES_USER=gorm -e POSTGRES_PASSWORD=gorm -e POSTGRES_DB=gorm \
  postgres:16-alpine

# Atlas owns the schema; the app never migrates
atlas migrate apply --config file://migrations/atlas.hcl --env gorm \
  --var dev_url="postgres://gorm:gorm@localhost:9921/gorm?sslmode=disable"

# Run the API + dispatcher
go run ./cmd/app -config config.yaml
# -> http://localhost:9999 (health: /health/live, /health/ready)
```

Without a config file, the same keys are read from `BEELINEWF_*` environment
variables (see [.env.example](.env.example)).

## Your first workflow

The annotated sample definition is [workflow.yaml](workflow.yaml).
`scripts/seed.sh` creates reusable node definitions plus a workflow from
them and prints their ids (defaults to the compose URL):

```bash
bash scripts/seed.sh http://localhost:8080
# -> {"form_id": "<FORM_ID>", "node_definition_ids": {...}, "workflow_definition_id": "<WF_ID>",
#     "public_workflow_definition_id": "<PUBLIC_WF_ID>"}

# Start an instance (202 Accepted)
curl -s -X POST http://localhost:8080/v1/workflow/instance \
  -H 'Content-Type: application/json' \
  -d '{"workflow_definition_id":"<WF_ID>","context":{"user":{"name":"Jono","title":"Manager"}}}'

# Poll status until it parks on input
curl -s http://localhost:8080/v1/workflow/instance/<INSTANCE_ID>/status

# Deliver input (202 Accepted; Idempotency-Key required)
curl -s -X PUT http://localhost:8080/v1/workflow/instance/<INSTANCE_ID>/input \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: first-try' \
  -d '{"title":"Approved"}'

# Inspect results
curl -s http://localhost:8080/v1/workflow/instance/<INSTANCE_ID>/status
curl -s http://localhost:8080/v1/workflow/instance/<INSTANCE_ID>/context
```

`scripts/e2e.sh [BASE_URL] [WORKFLOW_JSON]` runs black-box API checks
against a running app (pass an explicit workflow JSON file).

`scripts/e2e-oidc.sh [APP_BASE_URL] [OIDC_ISSUER_URL]` does the same for
authentication and authorization. It starts a throwaway mock identity
provider (real RSA key, real JWKS) and drives the app the way a frontend
would: a public login contract, a 401 with no credential, a token resolving
to a stable user, the role catalog refusing a role without `roles:read`, a
caller holding `input:deliver` still refused by a node that lists a
different role, the same caller accepted once the role matches, the
`record_actor` envelope recording the deliverer, and the `public: true`
path: anonymous delivery accepted on a public node, refused on a private one,
an invalid credential refused on both, and an anonymous status read still
401. Set `BEELINEWF_API_TOKEN` to also exercise the service-principal bypass.
The app must be started with a role seed that grants the roles the script
uses; `config.e2e-oidc.yaml` is a working example.

## Node types

| Type             | Does                                                                               | Details                            |
| ---------------- | ---------------------------------------------------------------------------------- | ---------------------------------- |
| `script`         | Goja ES5.1 transform, return value written to `output_property`                    | [workflow.yaml](workflow.yaml)     |
| `conditions`     | Evaluates all conditions, routes on the single match (0 or 2+ fail)                | [ARCHITECTURE.md](ARCHITECTURE.md) |
| `input`          | Parks with `waiting_reason: input` until a payload arrives                         | [API reference](#api-reference)    |
| `output`         | Publishes `context_path` JSON to Redis or RabbitMQ, returns receipt                | [workflow.yaml](workflow.yaml)     |
| `group`          | Nested sub-graph with its own key routing and hooks                                | [ARCHITECTURE.md](ARCHITECTURE.md) |
| `external_call`  | Outbound HTTP (allowlisted) or allowlisted command, optional `on_failure` fallback | [Configuration](#configuration)    |
| `poller`         | Repeats HTTP/Redis/RabbitMQ reads until `until` returns `true`                     | [ARCHITECTURE.md](ARCHITECTURE.md) |
| `parallel_start` | Forks named branches from a frozen parent snapshot, parks on the join              | [ARCHITECTURE.md](ARCHITECTURE.md) |
| `parallel_end`   | Barrier join: runs `combining_script` over branch contexts, continues past         | [ARCHITECTURE.md](ARCHITECTURE.md) |
| `delay`          | Explicit timed wait on arrival, then continues at `next_node`                      | [docs/integration/fields.md](docs/integration/fields.md) |

Every node type accepts `pre_script`/`post_script` hooks. Hook return
values are ignored; only context mutations persist. Script return values
and context mutations must be JSON-serializable — anything else (e.g. a
function value) fails the node with an error and leaves the stored
context untouched. Custom node types can
be added without core edits — see [docs/custom-nodes.md](docs/custom-nodes.md)
and the worked `s3fetch` example ([pkg/customnode/s3fetch/README.md](pkg/customnode/s3fetch/README.md)). See
[ARCHITECTURE.md](ARCHITECTURE.md) for hook ordering, poller transports
and defaults, and `on_failure` payload shape.

## Configuration

`config.yaml` holds infra, worker pool, engine limits, auth, and the
system audit user. Key settings:

| Key                                                         | Default                | Notes                                                                                                                          |
| ----------------------------------------------------------- | ---------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `infra.http.host`                                           | `localhost:9999`       | Compose overrides to `0.0.0.0:8080`                                                                                            |
| `infra.http.swagger_enabled`                                | `true`                 | Serves UI at `/swagger/index.html`                                                                                             |
| `infra.http.metrics_enabled`                                | `true`                 | Serves Prometheus exposition at `/metrics` (`BEELINEWF_INFRA_HTTP_METRICS_ENABLED`; `false` answers 404)                       |
| `infra.http.max_body_bytes`                                 | `1048576` (1 MiB)      | JSON body cap for context replace + input delivery; breach answers 413 (`BEELINEWF_INFRA_HTTP_MAX_BODY_BYTES`)                 |
| `infra.http.read_header_timeout`                            | `10s`                  | Max wait for request headers; closes slow-header connections (`BEELINEWF_INFRA_HTTP_READ_HEADER_TIMEOUT`)                      |
| `infra.http.idle_timeout`                                   | `60s`                  | Idle keep-alive cleanup; no write timeout so SSE streams are never cut (`BEELINEWF_INFRA_HTTP_IDLE_TIMEOUT`)                   |
| `infra.http.cors.allowed_origins`                           | `"*" (allow all)`      | Origin list to restrict; empty file value disables (`BEELINEWF_INFRA_HTTP_CORS_ALLOWED_ORIGINS`)                               |
| `infra.http.cors.allowed_methods`                           | `GET, POST, ...`       | PUT, DELETE, OPTIONS also allowed by default (`BEELINEWF_INFRA_HTTP_CORS_ALLOWED_METHODS`)                                     |
| `infra.http.cors.allowed_headers`                           | `Authorization, ...`   | Content-Type and X-Api-Token also allowed by default (`BEELINEWF_INFRA_HTTP_CORS_ALLOWED_HEADERS`)                             |
| `infra.http.cors.exposed_headers`                           | `[]`                   | Response headers browsers may read (`BEELINEWF_INFRA_HTTP_CORS_EXPOSED_HEADERS`)                                               |
| `infra.http.cors.allow_credentials`                         | `false`                | Credentialed requests; needs explicit origins (`BEELINEWF_INFRA_HTTP_CORS_ALLOW_CREDENTIALS`)                                  |
| `infra.http.cors.max_age`                                   | `12h`                  | Preflight cache lifetime (`BEELINEWF_INFRA_HTTP_CORS_MAX_AGE`)                                                                 |
| `infra.postgresql.dsn`                                      | local `gorm` DSN       | pgx/postgres wire format                                                                                                       |
| `infra.postgresql.max_open_conns`                           | `25`                   | Per-replica pool ceiling; exhaustion queues (`BEELINEWF_INFRA_POSTGRESQL_MAX_OPEN_CONNS`)                                      |
| `infra.postgresql.max_idle_conns`                           | `25`                   | Warm idle cap per replica (`BEELINEWF_INFRA_POSTGRESQL_MAX_IDLE_CONNS`)                                                        |
| `infra.postgresql.conn_max_lifetime`                        | `5m`                   | Max connection age before recycle (`BEELINEWF_INFRA_POSTGRESQL_CONN_MAX_LIFETIME`); `0` = unlimited                            |
| `infra.postgresql.conn_max_idle_time`                       | `5m`                   | Max idle time before eviction (`BEELINEWF_INFRA_POSTGRESQL_CONN_MAX_IDLE_TIME`); `0` = unlimited                               |
| `infra.postgresql.connect_timeout`                          | `15s`                  | Startup ping budget; a blackhole DSN fails the boot fast (`BEELINEWF_INFRA_POSTGRESQL_CONNECT_TIMEOUT`)                        |
| `infra.redis.dsn`                                           | `""` (disabled)        | e.g. `redis://localhost:6379/0`; unreachable broker fails startup                                                              |
| `infra.rabbitmq.dsn`                                        | `""` (disabled)        | e.g. `amqp://beelinewf:beelinewf@localhost:5672/`; queues default to `beelinewf.input`, `beelinewf.output`, `beelinewf.status` |
| `engine.default_node_timeout` / `max_node_timeout`          | `30s` / `5m`           | Caps script, `external_call`, and `output` nodes                                                                               |
| `engine.condition_timeout`                                  | `5s`                   | Fixed budget for conditions, input validation, poller predicates, join scripts                                                 |
| `engine.parallel.max_depth`                                 | `4`                    | Max parallel nesting depth (groups add no depth)                                                                               |
| `engine.parallel.max_branches_per_parallel`                 | `32`                   | Max branches on one `parallel_start` (min 2)                                                                                   |
| `engine.parallel.max_active_branches_per_instance`          | `128`                  | Max live branches per instance across all executions                                                                           |
| `engine.http_allowlist`                                     | loopback + examples    | `"*"` allows any target (development only, logs a warning)                                                                     |
| `engine.exec_allowlist`                                     | `/bin/echo`, `/bin/ls` | Absolute paths only (bare names fail startup); direct argv, never a shell                                                      |
| `engine.http_max_in_flight`                                 | `128`                  | Process-wide outbound HTTP cap shared by nodes, pollers, publishers, custom nodes                                              |
| `engine.http_max_in_flight_per_host`                        | `16`                   | Per-host outbound HTTP cap; fleet-wide concurrency is replicas x this                                                          |
| `auth.enabled` / `api_token`                                | `false`                | When enabled, `/v1` requires a credential: `X-Api-Token`, OIDC, or local (at least one must be configured)                     |
| `auth.oidc.*`                                               | disabled               | OIDC resource server; needs `issuer` and `client_id`                                                                           |
| `auth.local.*`                                              | disabled               | Built-in register/login with HS256 JWTs; needs `jwt_secret` (≥32 bytes, `BEELINEWF_AUTH_LOCAL_JWT_SECRET`)                     |
| `auth.role_permissions` / `BEELINEWF_AUTH_ROLE_PERMISSIONS` | file / `{}`            | First-boot seed for the role tables as YAML map or JSON object; env overrides file; invalid JSON fails startup                 |

List-valued keys accept comma-separated env values, e.g.
`BEELINEWF_ENGINE_HTTP_ALLOWLIST="api.example.com,jsonplaceholder.typicode.com"`,
`BEELINEWF_INFRA_HTTP_CORS_ALLOWED_ORIGINS="https://app.example.com,http://localhost:5173"`.
`BEELINEWF_AUTH_ROLE_PERMISSIONS` takes a JSON object of role name to action
list, e.g. `{"admin":["definitions:read","roles:read"]}`. A set value
overrides the config file; blank falls back to it. Invalid JSON fails
startup with a `configuration: BEELINEWF_AUTH_ROLE_PERMISSIONS: invalid JSON`
error. The seed applies only while the `roles` table is empty: once seeded,
the database is the source of truth and the API owns the catalog.

A browser frontend on another origin needs CORS: the default `"*"`
allows any origin, which suits development. For production, set
`infra.http.cors.allowed_origins` to the frontend's origin
(`scheme://host[:port]`), or to an empty list — in the config file only,
since an empty env value falls back to the default — to emit no CORS
headers. `"*"` cannot be combined with `allow_credentials: true`;
credentialed responses must name the origin.

## Authentication and authorization

Three independent credentials, and a role catalog on top of them.

### Authenticating

**OIDC bearer token (humans).** Set `auth.oidc.enabled`, `auth.oidc.issuer`,
and `auth.oidc.client_id`. The engine is a *resource server only*: it never
issues tokens, never redirects, and holds no client secret. A frontend reads
`GET /v1/auth/config` (public, since that is exactly the request made before
holding a token) to learn the issuer, client id, endpoints, and roles claim,
then runs authorization-code + PKCE against the provider and calls the API
with `Authorization: Bearer <jwt>`. OIDC and local logins coexist: a token
is routed to its verifier by issuer, so one deployment serves both.

The engine validates each token itself: discovery from the issuer, JWKS
signature check, `iss`, `aud`, and `exp` with a configurable
`auth.oidc.clock_skew`. No session, no cookie, no callback route.
Startup discovery is bounded by `auth.oidc.discovery_timeout` (default
`10s`), so an unreachable issuer fails the boot fast.
`auth.enabled` is the master switch: when it is false, `auth.oidc.enabled`
is forced off, so a stray OIDC setting can neither protect `/v1` nor trip
startup validation.

```bash
curl http://localhost:9999/v1/auth/config
curl -H "Authorization: Bearer $TOKEN" http://localhost:9999/v1/auth/me
```

**Local login (humans without a provider).** Set `auth.local.enabled` and
`auth.local.jwt_secret` (a dedicated HMAC secret of at least 32 bytes, via
`BEELINEWF_AUTH_LOCAL_JWT_SECRET`; never the API token or the at-rest
encryption key, and identical on every replica). Users register over
`POST /v1/auth/register` — while `auth.local.allow_registration` is false
(the default) only an admin creates accounts over `POST /v1/users` — and
log in over `POST /v1/auth/login`, which returns an HS256 JWT for the
`Authorization` header. A verified JWT authenticates exactly like an OIDC
one: same gates, same `/v1/auth/me`, same audit actor.
| GET        | `/v1/users`                                      | User directory, newest first (paged, needs `users:read`)                                                         |
| POST       | `/v1/users`                                      | Create local user (201, needs `users:write`)                                                                     |
| GET/PUT    | `/v1/users/{id}`                                 | Get user / update profile (PUT: `users:write` edits all, self edits name/email/password)                         |
| DELETE     | `/v1/users/{id}`                                 | Disable user (204, needs `users:write`)                                                                          |
| POST       | `/v1/users/{id}/resend-verification`             | Admin resend of the verification link (204, needs `users:write`)                                                 |
| POST       | `/v1/auth/register`                              | Self-register (201; pending with no token while verification is on)                                              |
| POST       | `/v1/auth/login`                                 | Log in, HS256 JWT (200; generic 401, 403 while pending)                                                          |
| POST       | `/v1/auth/verify-email`                          | Consume a verification link, returns a session (200)                                                             |
| POST       | `/v1/auth/resend-verification`                   | Resend a verification link (200, always generic)                                                                 |

```bash
curl http://localhost:9999/v1/auth/config   # local_enabled, registration_enabled, ...
curl -s -X POST http://localhost:9999/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"name":"Ada","email":"ada@example.test","password":"correct horse battery staple"}'
TOKEN=$(curl -s -X POST http://localhost:9999/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.test","password":"correct horse battery staple"}' | jq -r .access_token)
curl -H "Authorization: Bearer $TOKEN" http://localhost:9999/v1/auth/me
```

Passwords are stored as bcrypt hashes and never returned by any API;
unknown email, wrong password, and disabled account share one generic 401
so login cannot probe for accounts.

The first admin is bootstrapped with the API token, the way any operator
would:

```bash
curl -s -X POST http://localhost:9999/v1/users \
  -H "X-Api-Token: $BEELINEWF_API_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"Root","email":"root@example.test","password":"...","roles":["admin"]}'
```

*Email verification (optional).* With
`auth.local.email_verification.enabled`, registration creates a pending
account that cannot log in (403) until its mailed link is consumed over
`POST /v1/auth/verify-email`, which returns a login session at once.
Enabling it requires `auth.local.public_base_url` (the frontend origin the
link points at: `{base}/verify-email?token=...`) and the
`auth.local.smtp.*` relay settings; the boot fails fast without them.
Tokens are single-use, expire after `token_ttl` (default 24h), and are
stored hashed — the raw token exists only in the email. Resends
(`POST /v1/auth/resend-verification`, or
`POST /v1/users/{id}/resend-verification` for an admin) pace per email
through `resend_cooldown`, and the public resend always answers the same
generic success. Admin-created accounts verify at write.

*Rate limits.* The four public endpoints — register, login,
resend-verification, verify-email — share one throttle
(`auth.local.rate_limit`, default 10/min, blocked 5m) pairing each
endpoint with the client IP and, except verify-email, the normalized
email. Only failures count on login, so good traffic never burns the
budget; past it the answer is 429 with `Retry-After`. Like the API-token
throttle it is per-replica in-memory state using the shared
`auth.rate_limit.trusted_proxies` list, so put a proxy-level throttle in
front of multi-replica deployments.

**API token (the service principal).** `auth.enabled` + `auth.api_token`
accepts `X-Api-Token`. This principal bypasses every authorization gate —
it exists so machines and the broker can drive instances. Treat it like a
password, and never hand it to a browser. Broker deliveries (Redis, RabbitMQ)
have no user identity at all and are likewise the service principal.

With both disabled, `/v1` behaves exactly as it did before this feature:
open.

### Who the caller is

Roles come from the live token (`auth.oidc.roles_claim`, default `roles`;
arrays, a single string, and space-delimited strings all work), never from
the database. A verified `(issuer, subject)` is just-in-time upserted into
`users` on first sight, so a token for someone the instance has never seen
still resolves to a stable `users.id` that later shows up in attribution.

Local users resolve differently: their roles live in `user_roles` rows,
read at login and embedded in the JWT, so a role change takes effect at
the next login. The resolve step still consults live state on every
request: a disabled account — or, while verification is on, an unverified
one — stops an otherwise valid token at once.

### Authorizing

The role tables are the source of truth; configuration seeds them on a first
boot:

```yaml
auth:
  role_permissions:
    admin: ["*"]
    finance: ["instances:read", "input:deliver"]
    auditor: ["definitions:read", "instances:read", "statistics:read", "roles:read"]
```

Same seed via env: `BEELINEWF_AUTH_ROLE_PERMISSIONS='{"admin":["*"],"finance":["instances:read","input:deliver"]}'` (overrides the file). The seed runs only while the `roles` table is empty: once seeded, editing config or env no longer changes the catalog, and a restart keeps the database state. Manage roles over the API instead (below).

`"*"` is the wildcard: a role holding it passes every gate, the same bypass
the service principal gets. The other two lines are the shipped defaults from
`config.yaml`; read them as a starting point rather than a recommendation,
because the point of a catalog is to grant the least each role needs.

Each route declares the one action it needs:

| Action                           | Covers                                           |
| -------------------------------- | ------------------------------------------------ |
| `definitions:read`               | node/workflow definition reads                   |
| `definitions:write`              | definition writes and deletes                    |
| `secrets:read` / `secrets:write` | secret reads / writes                            |
| `instances:create`               | instance creation                                |
| `instances:read`                 | status, context, node-debug, debug-context reads |
| `instances:update-context`       | context replacement                              |
| `input:deliver`                  | the input endpoint gate                          |
| `instances:control`              | pause, resume, stop, rollback                    |
| `instances:migrate`              | definition migration of a running instance       |
| `statistics:read`                | the statistics summary                           |
| `roles:read`                     | the role catalog                                 |
| `roles:write`                    | role creation, replacement, and deletion         |
| `users:read` / `users:write`     | user directory reads / local user CRUD           |

A role the token carries that is absent from the catalog grants nothing, so
unknown roles deny by default.

`GET /v1/roles` and `GET /v1/roles/{name}` need `roles:read`.
`POST /v1/roles` (201), `PUT /v1/roles/{name}` (200), and
`DELETE /v1/roles/{name}` (204) need `roles:write`; the API token bypasses
with no role grant, so it stays the recovery path. `PUT` fully replaces the
permission list — an empty list revokes every grant — and keeps the stored
description when omitted. Writes take effect immediately on the writing
replica; other replicas catch up within ~30s.

At least one role must always retain the `roles:write` permission, explicitly
or via `"*"`: stripping it from the last holder, or deleting that role, is
**409** and leaves the role unchanged. A deployment seeded before
`roles:write` existed grants it with one API-key write adding `roles:write`
to `admin`.

A deployment seeded before `users:write` existed grants it the same
way, adding `users:write` to `admin` — otherwise user CRUD stays 403
for every role but the service principal.

### Input delivery is gated by permission, user, then role

`PUT /v1/workflow/instance/{id}/input` applies, in order:

1. the endpoint permission `input:deliver`;
2. the input node's `allowed_users`, checked against the caller's user id;
3. the input node's `allowed_roles`, intersected with the caller's roles.

The two node gates are a union: when the node lists both users and roles, a
listed user passes however their roles read, and a listed role passes
however the user list reads. Only a caller matching neither list is refused
with **403**. A node carrying one gate consults only that gate, and a node
carrying neither gate is open to any `input:deliver` holder. A refusal
writes no delivery row and records an `input_forbidden` audit event without
the payload.

```yaml
- type: input
  id: "…"
  name: Approval
  channel: http
  output_property: approval
  allowed_roles: [manager]   # optional; absent or empty is open; a listed role passes even when allowed_users is set
  allowed_users: [{id: "…"}] # optional; a listed user passes however their roles read
  record_actor: true         # optional; defaults to false
```

`allowed_roles`, `allowed_users`, and `record_actor` belong to the node
definition, not to the occurrence. An occurrence that references a reusable
node definition cannot override them: it is rejected at parse time, because
the occurrence does not know its own type until the definition is
materialized, so a gate carried there would be silently dropped. Every
occurrence of a definition therefore shares one gate. To park the same input
node under a different gate, create a second node definition.

### Public input nodes accept anonymous delivery

`public: true` on an input node is the third option, and the only gate that
skips authentication entirely. It exists for webhooks: an outside service
posts a payload it cannot hold a credential for.

```yaml
- type: input
  id: "…"
  name: Partner webhook
  channel: http
  output_property: webhook
  public: true
```

A public node accepts a `PUT` carrying **no credential at all**, even with
authentication switched on. The delivery route is exempted from the global
auth gate to allow it, together with the two reads a credential-less
frontend needs: `GET /v1/forms/{id}` serves the render contract unchanged,
and `GET /v1/workflow/definition/{id}` serves a filtered projection of the
graph (node identities, edges, and input delivery contracts — no scripts,
configs, authorization gates, or audit fields). Everything else, including
the instance status and context reads, still requires a credential.

The flag is opt-in and per node, and it forces the delivery open and
unattributed: a public node ignores `allowed_roles` and `allowed_users`,
which stay legal on the definition and stay stored as authored, and only
`record_actor: true` is rejected at parse time with
`public input node must have record_actor=false`, since the envelope would
have no `user_id` to write. Status reports both gates empty on a public
node, so a frontend never hides a form that is actually open. The flag only
exists on input nodes, and, like the gates, it belongs to the node
definition rather than the occurrence.

Three rules are worth stating plainly:

- **Anonymous is refused on a private node.** A credential-less `PUT` to a
  node without `public` answers **403**; the service loads the parked node
  first and refuses before any delivery row is written.
- **A bad credential is still 401, even on a public node.** A caller that
  presented something wrong is never quietly downgraded to an accepted
  anonymous delivery.
- **An authenticated caller on a public node is unchanged.** It still passes
  the two gates normally and its delivery is attributed to it, so the flag
  widens who may knock, not who gets recorded.

An anonymous delivery is written as the bare payload (a public node cannot set
`record_actor`) and marked `delivered_by: anonymous` on its `input_received`
event, so the history tells an anonymous delivery apart from a
service-principal bypass and from a user delivery. Refusals carry the same
marker on their `input_forbidden` event.

Instance status reads stay authenticated, so share the instance id
out-of-band, the way you would share a webhook URL — but the caller fetches
the payload shape itself from the two public reads. The pending-input
contract reports `public`, so an authenticated frontend can tell before anyone
tries.

### Attribution

`record_actor: true` wraps the accepted payload in an envelope before it
reaches the context:

```json
{ "user_id": "…", "input_data": { "approved": true } }
```

So templates read `{{ approval.input_data.approved }}` and
`{{ approval.user_id }}`. The validation script and the post hook's
`output` always see the **raw** payload, so validation code is unaffected.
With `record_actor` absent or false the bare payload is written
and existing definitions behave exactly as before. A broker or API-token
delivery records the system user. Replaying an idempotency key returns the
first writer's recorded delivery, attribution included.

## API reference

The authoritative contract is [api/openapi.yaml](api/openapi.yaml)
(Swagger UI served from the app; regenerate committed `docs/` with
`task swagger` after annotation changes).

| Method     | Path                                               | Purpose                                                                                                             |
| ---------- | -------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| GET        | `/health/live`, `/health/ready`                    | Liveness / readiness (always public)                                                                                |
| GET        | `/metrics`                                         | Prometheus exposition (see Metrics)                                                                                 |
| GET        | `/v1/auth/config`                                  | Public login contract (OIDC + local)                                                                                |
| GET        | `/v1/auth/me`                                      | The authenticated caller (identity, roles, effective permissions)                                                   |
| GET        | `/v1/roles`                                        | Role catalog (needs `roles:read`)                                                                                   |
| GET        | `/v1/roles/{name}`                                 | One role and its permissions                                                                                        |
| POST       | `/v1/roles`                                        | Create role (201, needs `roles:write`)                                                                              |
| PUT        | `/v1/roles/{name}`                                 | Replace role permissions (200; 409 on last-holder strip)                                                            |
| DELETE     | `/v1/roles/{name}`                                 | Delete role (204; 409 on last-holder delete)                                                                        |
| POST       | `/v1/node/definition`                              | Create immutable node definition (201)                                                                              |
| GET        | `/v1/node/definition`                              | List (paged, `latest_only`, `type`, ...)                                                                            |
| GET/DELETE | `/v1/node/definition/{id}`                         | Get / delete (fails if referenced)                                                                                  |
| GET        | `/v1/node/types`                                   | Node type catalog (builtins + registered customs, each with its schema)                                             |
| POST       | `/v1/workflow/definition`                          | Create immutable workflow definition (201)                                                                          |
| GET        | `/v1/workflow/definition`                          | List (paged, `latest_only`, ...)                                                                                    |
| GET/DELETE | `/v1/workflow/definition/{id}`                     | Get (public filtered read) / delete (fails if referenced)                                                           |
| POST       | `/v1/secrets`                                      | Create secret (201; plaintext omitted)                                                                              |
| GET        | `/v1/secrets`                                      | List masked secrets (paged, ordered by key)                                                                         |
| GET/DELETE | `/v1/secrets/{key}`                                | Get masked secret / delete; duplicate create returns 409                                                            |
| PUT        | `/v1/secrets/{key}`                                | Replace secret value (404 unless the key exists)                                                                    |
| POST       | `/v1/workflow/instance`                            | Start instance (202)                                                                                                |
| GET        | `/v1/workflow/instance`                            | List compact summaries (paged, `id`/`workflow_definition_id`/`status` filters)                                      |
| GET        | `/v1/workflow/instance/{id}/status`                | Status, counters, cursor, per-node `nodes` map, audit actors                                                        |
| GET/PUT    | `/v1/workflow/instance/{id}/context`               | Get full context / replace it (paused only, 409 on race)                                                            |
| GET        | `/v1/workflow/instance/{id}/status/node/{node_id}` | Node debug (`?attempt=N`; `not_started` for never-run nodes)                                                        |
| GET        | `/v1/workflow/instance/{id}/debug/context`         | Debug context as TypeScript (debug runs only, `?node_id=`/`?attempt=N`)                                             |
| PUT        | `/v1/workflow/instance/{id}/input`                 | Deliver input (`Idempotency-Key` required, 202; 403 by permission or node gates, or anonymous on a non-public node) |
| POST       | `/v1/workflow/instance/{id}/pause`                 | Pause (200 immediate / 202 deferred)                                                                                |
| POST       | `/v1/workflow/instance/{id}/resume`                | Resume                                                                                                              |
| POST       | `/v1/workflow/instance/{id}/stop`                  | Terminal stop (fences workers, cancels in-flight execution)                                                         |
| POST       | `/v1/workflow/instance/{id}/rollback`              | Roll back paused/failed instance to a prior occurrence (lands paused)                                               |

Instance statuses: `waiting`, `running`, `paused`, `finished`, `failed`,
`stopped`. Errors follow RFC 7807 `problem+json`. A missing or invalid
credential is **401**; a valid caller without the route's permission, or one
refused by an input node's user or role gate, is **403**. The exceptions are
the public-input reads — `GET /v1/forms/{id}` and the filtered
`GET /v1/workflow/definition/{id}` — plus `PUT …/input` on a node with
`public: true`, all reachable with no credential at all; see
[Public input nodes](#public-input-nodes-accept-anonymous-delivery).

### Secrets and templates

Create a secret before starting an instance:

```bash
curl -s -X POST http://localhost:8080/v1/secrets \
  -H 'Content-Type: application/json' \
  -d '{"key":"API_KEY","value":"replace-me"}'
# 201: {"key":"API_KEY","value_masked":"********",...}

# Rotate the value later (404 unless the key exists):
curl -s -X PUT http://localhost:8080/v1/secrets/API_KEY \
  -H 'Content-Type: application/json' \
  -d '{"value":"rotated"}'
# 200: {"key":"API_KEY","value_masked":"********",...}
```

At instance creation, all stored secrets are snapshotted under the reserved
`secret` context root. Workflow and node templates can resolve
`{{ secret.API_KEY }}` through the normal context-path renderer. The
snapshot is frozen: later secret deletion or rotation does not change an
existing instance. Context, status, and node-debug read APIs redact stored
secret values and any rendered copies in debug input, output, or error
text.

## Status notifications

A definition opts in with a top-level `status_update` block (any
combination of transports, each with its own retry policy):

```yaml
status_update:
  http:
    url: "https://example.com/workflow-status"
    max_retry: 3
    retry_delay: "5s"
  redis:
    max_retry: 2
  rabbitmq:
    max_retry: 1
```

Covered transitions: `waiting_for_input`, `input_received`, `paused`,
`resumed`, `finished`, `failed`, `stopped`. Each event fans out to one
outbox row per configured transport sharing one logical event id, delivered
in per-instance/per-transport order, at-least-once. Receivers dedupe on the
id (`Idempotency-Key` / `X-BeelineWF-Event-ID` on HTTP, AMQP `message_id` on
RabbitMQ, embedded `id` on Redis). A transport configured without its
broker DSN dead-letters. Full payload and ordering guarantees are in
[ARCHITECTURE.md](ARCHITECTURE.md).

## Metrics

`GET /metrics` serves Prometheus exposition (`text/plain; version=0.0.4`)
from the same Gin engine and port as the API — no separate metrics port.
It carries Go/process series plus HTTP RED and the domain series below.

Toggle: `infra.http.metrics_enabled` (default `true`,
`BEELINEWF_INFRA_HTTP_METRICS_ENABLED`). `false` leaves no route, so
`/metrics` answers `404` with or without a token.

Auth follows the existing switch. With auth disabled, `/metrics` is public
like `/health/live`; restrict it at the network layer in production. With
auth enabled, only `X-Api-Token` equal to `auth.api_token` passes — OIDC
`Bearer` alone is `401`, and auth enabled with no token configured is `401`
for every request (fail closed). A `401` body carries no series. Rotating
`BEELINEWF_API_TOKEN` rotates scrape access.

```bash
curl -s http://localhost:9999/metrics | head
curl -s -H "X-Api-Token: $BEELINEWF_API_TOKEN" http://localhost:9999/metrics | head
```

```yaml
# prometheus.yml: inject the token from a secret, never a committed literal.
scrape_configs:
  - job_name: wf-engine
    static_configs:
      - targets: ["wf-engine:8080"]
    http_headers:
      X-Api-Token:
        credentials:
          file: /run/secrets/beelinewf_api_token
```

Series (labels are bounded enums and route templates only — never an
instance ID, definition ID, user, host URL, or raw request path):

| Series | Type / labels | Scaling decision it informs |
| ------ | ------------- | --------------------------- |
| `http_requests_total`, `http_request_duration_seconds` | counter / histogram `{method,route,status}` | API replica count; `route` is the Gin template |
| `wf_instances_created_total` | counter | Inbound demand rate |
| `wf_instances_terminal_total{status}` | counter (`finished`/`failed`/`stopped`) | Completion rate, error budget |
| `wf_instances_active{status}` | gauge (cached snapshot) | Concurrency headroom vs `worker.pool.size` |
| `wf_instance_duration_seconds{status}` | histogram | End-to-end capacity; tail-latency regressions |
| `wf_node_executions_total{node_type,result}` | counter (`success`/`error`/`timeout`) | Which node types drive load/failure |
| `wf_node_execution_duration_seconds{node_type}` | histogram | Worker sizing; slow-node detection |
| `wf_dispatcher_backlog` | gauge (cached) | Add replicas / raise claim batch — work waiting, not running |
| `wf_dispatcher_oldest_pending_age_seconds` | gauge (cached) | Starvation alert; backlog growth vs slowness |
| `wf_dispatcher_claims_total` | counter | Dispatcher throughput per replica |
| `wf_status_outbox_backlog` | gauge (cached) | Status-dispatcher scaling (`worker.status_pool.size`) |
| `wf_status_deliveries_total{transport,result}` | counter (`success`/`error`) | Status delivery health per transport |
| `wf_scheduler_fires_total{result}` | counter (`fired`/`failed`/`skipped`) | Scheduler load; missed/failed fires |
| `wf_broker_consumer_healthy{transport}` | gauge 0/1 | Broker consumer liveness without scraping `/health/ready` |
| `wf_http_bulkhead_inflight` | gauge (global scope) | `engine.http_max_in_flight` tuning |
| `wf_db_pool_open_connections`, `wf_db_pool_in_use`, `wf_db_pool_wait_count_total`, `wf_db_pool_wait_duration_seconds_total` | gauges/counters from `sql.DB.Stats()` | `infra.postgresql.max_open_conns` sizing |

Scaling runbook:

- Backlog + oldest-age rising while node durations stay flat → add
  replicas / raise the worker pool; work is waiting, not slow.
- DB wait count rising → raise `infra.postgresql.max_open_conns`.
- Bulkhead inflight pinned at cap → raise `engine.http_max_in_flight`.
- Terminal `failed` rate rising with node `error` on one type → that node
  type is the fault, not capacity.

DB-backed gauges serve a TTL-cached snapshot (15s), so scrape load is flat
regardless of Prometheus interval; treat them as up-to-15s stale, never
real-time. `/metrics` is intentionally absent from OpenAPI/Swagger: the
exposition is `text/plain`, not a JSON contract.

## Development

```bash
task test        # full suite (needs PostgreSQL on :9921, see Testing below)
task test-race   # full suite under the race detector
task lint        # golangci-lint
task vet         # go vet
task e2e         # black-box API checks against a running app
task swagger     # regenerate docs/ from API annotations
task migrate-up      # atlas migrate apply
task migrate-diff    # new migration from GORM models
task migrate-lint    # validate migration directory
```

Testing contract: the full suite needs PostgreSQL. One `TEST_DATABASE_DSN`
auto-creates the six per-suite scratch databases on first run (the role needs
CREATEDB once; afterwards the existence check is a no-op). Without Postgres,
`go test -short ./...` skips the DB suites loudly instead of failing. Local
Postgres one-liner:

```bash
docker run -d --name beelinewf-pg -e POSTGRES_USER=gorm -e POSTGRES_PASSWORD=gorm -e POSTGRES_DB=gorm -p 9921:5432 postgres:16
```

Validation before opening a PR:

```bash
go run mvdan.cc/gofumpt@v0.12.0 -l .
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
golangci-lint run ./...
atlas migrate validate --config file://migrations/atlas.hcl --env gorm \
  --var dev_url="postgres://gorm:gorm@localhost:9921/gorm?sslmode=disable"
```

CI (`.github/workflows/`) runs gofumpt, vet, tests, race tests, lint, and
Atlas validate plus apply on pushes to `main`/`master` and on pull requests. A
coverage workflow tracks a 75% total floor plus per-package floors for the
DB suites, and the badge at the top of this
file is refreshed automatically; leave that badge line in place.

## Project layout

```
cmd/app                    composition root (config, repos, engine, dispatcher, router)
cmd/atlas-loader           Atlas schema loader (app never migrates)
internal/workflow/model       domain entities, statuses, state machine invariants
internal/workflow/repository  GORM models, fenced claims, checkpoints, outbox
internal/workflow/executor    Goja sandbox, node executors, lifecycle hooks
internal/workflow/engine      cursor machine, dispatcher, cancellation registry
internal/workflow/transport   optional Redis/RabbitMQ adapters
internal/workflow/inputtransport  broker input consumers
internal/workflow/statusupdate    outbox dispatcher + status publishers
internal/workflow/service     use-case orchestration (instances, controls, debug)
internal/workflow/handler     Gin routes, DTOs, problem+json
pkg/*                      config, database, ids, context paths (no internal imports)
api/openapi.yaml           authoritative API contract
docs/                      generated Swagger docs (via task swagger)
migrations/                Atlas config + versioned SQL
workflow.yaml              annotated sample workflow definition
scripts/                   seed.sh (sample seed), e2e.sh (black-box checks),
                           e2e-oidc.sh (auth/RBAC/input-gate checks)
config.e2e-oidc.yaml       app config for scripts/e2e-oidc.sh
```

## Further reading

- [ARCHITECTURE.md](ARCHITECTURE.md): package boundaries, runtime model,
  state machines, recovery, hooks, failure routing, notifications.
- [api/openapi.yaml](api/openapi.yaml): authoritative REST contract.
- [workflow.yaml](workflow.yaml): annotated sample definition covering all
  node types.
- [.env.example](.env.example): every setting as environment variables.
- [docker-compose.yml](docker-compose.yml): full local stack wiring.

## Contributing

1. Fork the repo and create a feature branch from `main`.
2. Keep changes surgical: touch only what the task needs, match existing
   style, and add tests for new behavior.
3. Run the validation block above (`gofumpt`, `vet`, tests, `lint`; Atlas
   validate if migrations changed; `task swagger` if API annotations
   changed).
4. Open a pull request against `main` describing what changed and how it
   was verified. CI must pass.

Good first checks on any PR: the gofumpt check prints nothing,
`go vet ./...` is clean, and `task test` passes.

## Security

- Never use `engine.http_allowlist: ["*"]` outside local development; every
  request and redirect is validated against the allowlist (scheme,
  host:port, DNS).
- Keep `engine.exec_allowlist` minimal and absolute-path-only (bare names
  fail startup). Commands run directly (no shell) and are killed as a
  process group on timeout or cancel, but each entry is still arbitrary
  code execution by design.
- Enable `auth.enabled` and set a strong `api_token` for any shared or
  production deployment. Treat tokens like passwords.
- The API token is the **service principal** and bypasses every authorization
  gate by design, so it is equivalent to full administrative access for any
  workflow. Never send it from a browser or ship it in a frontend bundle; use
  it only from trusted machine callers. Enabling OIDC alone does not restrict
  it.
- `GET /v1/auth/config` is intentionally public and returns no secret: it
  carries only the issuer, client id, endpoints, and roles claim, which the
  provider publishes anyway, plus which local logins exist.
- The single form and workflow-definition reads are public alongside it, so
  a public-input frontend can fetch the shape with no credential. The form
  body is the render contract itself; the definition body is a filtered
  projection (graph and input delivery contracts only — no scripts,
  configs, gates, or audit fields). Anyone holding an id can confirm it
  exists, so treat ids as unguessable but not secret.
- `auth.local.jwt_secret` and `auth.local.smtp.password` are secrets: ship
  them via env (`BEELINEWF_AUTH_LOCAL_JWT_SECRET`,
  `BEELINEWF_AUTH_LOCAL_SMTP_PASSWORD`), never in a committed file. The JWT
  secret signs every local session — rotate it and every outstanding token
  dies; the SMTP password authenticates the verification relay.
- Local passwords are stored as bcrypt hashes and are never returned by any
  API; verification tokens are stored hashed too, and the raw token exists
  only in the email. Login and resend answers are generic by design, so a
  refusal cannot be read as an account-existence oracle.
- Keep `auth.local.allow_registration` closed unless the deployment is
  meant to be self-service; an open registration is an account for anyone
  who can reach the port.
- `public: true` on an input node is the one place the API serves an
  unauthenticated **write**, so treat it as a public endpoint you own. Anyone
  holding the instance id can deliver to it, the payload is stored as-is, and
  the run continues. Set it only where an outside party must post to you, and
  pair it with a strict validation script: the flag is open by construction,
  so the script is the only backend gate on what a stranger can write into
  your context. Prefer an authenticated route and a signature check when you
  control both ends.
- Roles are taken from the verified token, never from the request body, so a
  caller cannot escalate by claiming a role in a parameter. A role absent
  from the catalog grants nothing.
- Found a vulnerability? Please use GitHub's private vulnerability
  reporting on this repository instead of opening a public issue, so a fix
  can land before disclosure.

## License

MIT. See [LICENSE](LICENSE).

Copyright (c) 2026 BeelineWF.

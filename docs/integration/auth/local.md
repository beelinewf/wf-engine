# Built-in local auth for BeelineWF (no external provider)

For deployments without an identity provider, the API registers users, logs
them in, and mints its own HS256 JWTs. A verified local JWT authenticates
exactly like an OIDC one: same `Authorization: Bearer <jwt>` header, same
permission gates, same `GET /v1/auth/me`, same audit actor. OIDC and local
logins coexist: a token is routed to its verifier by issuer.

This guide is the operator + frontend contract. Endpoint shapes live in
`endpoints.md` (`Auth` section); the authoritative field contract is
`api/openapi.yaml`.

## 1. Enablement

```yaml
auth:
  enabled: true
  local:
    enabled: true
    jwt_secret: "" # via BEELINEWF_AUTH_LOCAL_JWT_SECRET, >= 32 bytes, same on every replica
    token_ttl: 24h
```

`auth.enabled` is the master switch: with it off, local auth is forced off.
With it on, at least one credential must exist (API token, OIDC, or local),
or the boot fails. The JWT secret is dedicated — never the API token or the
at-rest encryption key. Rotating it kills every outstanding local session at
once, by design. `token_ttl` bounds a session; there is no refresh token in
v1, so an expired session re-logs-in. When OIDC runs alongside local auth,
`auth.local.issuer` must differ from `auth.oidc.issuer`: tokens route to
their verifier by issuer, and a shared value would be ambiguous — the boot
refuses it.

First admin (operator, with the API token):

```bash
curl -s -X POST http://localhost:9999/v1/users \
  -H "X-Api-Token: $BEELINEWF_API_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"Root","email":"root@example.test","password":"...","roles":["admin"]}'
```

`roles` must name catalog roles. The response never carries the credential.

## 2. Registration posture

`auth.local.allow_registration` (default `false`) opens
`POST /v1/auth/register`. Closed means accounts arrive from an admin only
(`POST /v1/users`, needs `users:write`); open means anyone who can reach
the port gets an account, so keep it closed unless the deployment is meant
to be self-service. An open registration assigns
`auth.local.default_role` (empty assigns no roles, and is the default);
never auto-admin. A duplicate email is `409` — which confirms the address
holds an account. That oracle is the accepted price of open registration
(login and resend stay generic); the throttle below bounds probing, and a
deployment that must not leak membership keeps registration closed and
provisions through `POST /v1/users` instead.

Passwords are 6–72 characters with at least one uppercase letter, one
number, and one symbol, stored as bcrypt hashes, never returned.

## 3. Email verification

Off by default. With `auth.local.email_verification.enabled`:

- `POST /v1/auth/register` → `201` with `pending_verification: true` and
  **no token**. The account exists but cannot log in (`403`) until its
  mailed link is consumed.
- The link points at the **frontend**: `{public_base_url}/verify-email?token=...`
  (`auth.local.public_base_url` is required). The frontend page reads the
  `token` query param and `POST`s it to `/v1/auth/verify-email`, which
  returns a login session at once — no second login needed.
- Tokens are single-use and expire after `token_ttl` (default 24h).
  Unknown is `422`, expired asks for a resend.
- `POST /v1/auth/resend-verification` always answers the same generic
  `200`, for unknown, verified, and pending emails alike. Sends pace per
  email through `resend_cooldown` (default 60s).
- `POST /v1/users/{id}/resend-verification` (needs `users:write`) is the
  admin variant: unknown id is `404`, the cooldown does not apply.
- An email change (self or admin) re-pends the account and mails the new
  address, unless the admin explicitly verifies in the same call.
  Admin-created accounts verify at write.

SMTP (`auth.local.smtp.*`: host, port, username, password, from, use_tls)
delivers the mail and is required when verification is on — the boot fails
fast without it. `smtp.host: "log"` selects the dev sender that logs the
link instead of sending it. The compose dev stack instead wires a bundled
Mailpit (`mailpit:1025`, UI at `http://<host>:18025`) so verification mail
lands somewhere readable, with verification and open registration switched
on for the end-to-end exercise. TLS negotiates at 1.2 minimum and never
downgrades; `use_tls: false` against anything but localhost logs a warning
because the link travels in cleartext. Each send carries a 10s deadline, so
a stalled relay slows one request instead of hanging it. Stored tokens are
SHA-256 hashes; the raw token exists only in the email.

The mail ships as `multipart/alternative` from embedded templates: a
styled HTML part plus a plain-text fallback, with the link, a humanized
expiry ("24 hours"), and an ignore-if-not-you line. `auth.local.smtp.template_dir`
overrides both with `verify_email.html` and `verify_email.txt` from one
directory; the pair renders with `.Link` and `.ExpiresIn`, the HTML through
`html/template` auto-escaping. The dir is all-or-nothing — a missing file
or a broken template fails the boot, never the first registration. The
subject (`Verify your email`) is not templated.

`public_base_url` must be an absolute `http(s)` URL with a host — the boot
refuses a bare hostname or a non-web scheme, since the value is mailed to
strangers and lands in an `href`.

Frontend flow: read `GET /v1/auth/config` first. When `local_enabled` is
true, show the login form; when `registration_enabled` is true, show
register; when `email_verification_enabled` is true, a pending register
response routes to a "check your mailbox" state instead of a session.

## 4. Login and sessions

`POST /v1/auth/login` with `{email, password}` → `200` with
`{access_token, token_type: "Bearer", expires_in, user}`. Unknown email,
wrong password, and disabled account share one generic `401`, so the
endpoint cannot probe for accounts; a pending account is `403`.

`PUT /v1/users/{id}` is self-service for profile fields: the account
itself edits name, email, and password only (a rotation proves
`current_password`), while a `users:write` holder edits every field.
`DELETE /v1/users/{id}` disables the account (needs `users:write`): the
row survives for the audit trail, but the credential is cleared and
outstanding tokens stop resolving at once. The system user and the
caller's own account refuse with `422`.

Role changes take effect at the next login (roles are embedded in the JWT
at login time, mirroring OIDC). Disable and un-verification take effect
immediately: the resolve step checks live state on every request.

## 5. Rate limits

The four public endpoints (register, login, resend-verification,
verify-email) share one throttle (`auth.local.rate_limit`, default
10/min, blocked 5m) and answer `429` with a `Retry-After` header past it.
Buckets pair each endpoint with the client IP and — except verify-email,
which sees no email before the token — with the normalized email, so one
attacker's block never silences another mailbox. Only failures count on
login: successes neither burn the budget nor extend a block, and a success
clears the email's failures. Back off on `429` and honor `Retry-After`;
the body is identical across endpoints and cases, so it says nothing
about whether an account exists. The throttle is per-replica in-memory
state — behind multiple replicas, add a proxy-level limit as well.

## 6. Upgrading a deployment seeded before `users:write`

The role tables seed once: adding `users:write` to config does not grant
it on an upgraded install. Grant it with one API-key write adding
`users:write` to `admin` (`PUT /v1/roles/admin`), or user CRUD stays `403`
for every role but the service principal.

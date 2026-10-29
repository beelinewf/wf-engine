#!/usr/bin/env bash
# BeelineWF end-to-end check of the built-in local user flow, against a
# running app wired to config.e2e-local.yaml (local auth on, log mail
# sender, API token on).
#
#   E2E_LOCAL_MODE=closed bash scripts/e2e-local.sh [APP_BASE_URL]
#   E2E_LOCAL_MODE=open   bash scripts/e2e-local.sh [APP_BASE_URL]
#
# Closed mode (registration off, verification off) drives the operator path
# the way a first boot would:
#
#   * the login contract reports local auth without registration;
#   * self-registration is refused 403;
#   * the API token bootstraps the first admin over POST /v1/users, and the
#     created body carries no password_hash;
#   * login returns a JWT that resolves over /v1/auth/me and passes a
#     role-gated route;
#   * a wrong password is 401;
#   * deleting the user revokes the JWT: the same token is 401 afterwards.
#
# Open mode (registration on, verification on) drives the mailbox path:
#
#   * registration pends with no token, and login before verification is 403;
#   * a wrong password is 401 and a garbage verification token is 422;
#   * resend answers the identical generic body for an unknown and a pending
#     email;
#   * the logged link verifies the account and returns a login session;
#   * a duplicate registration is 409;
#   * flooding login trips 429 with a Retry-After.
#
# BEELINEWF_API_TOKEN must name the same credential the app was started with;
# the task target exports it once for both. APP_LOG must point at the app's
# log file in open mode, so the script can lift the verification link the
# log sender wrote there.

set -euo pipefail

APP_BASE_URL="${1:-http://127.0.0.1:9998}"
MODE="${E2E_LOCAL_MODE:-open}"

fail() { echo "e2e-local: FAIL: $*" >&2; exit 1; }
pass() { echo "e2e-local: ok: $*"; }

[[ -n "${BEELINEWF_API_TOKEN:-}" ]] \
  || fail "BEELINEWF_API_TOKEN must be set: the admin-bootstrap checks are mandatory"

auth_header() { printf 'Authorization: Bearer %s' "$1"; }

if [[ "$MODE" == "closed" ]]; then
  # --- the login contract reports local auth without registration ------------
  cfg=$(curl -fsS "$APP_BASE_URL/v1/auth/config") || fail "read auth config without a credential"
  [[ "$(printf '%s' "$cfg" | jq -r '.local_enabled')" == "true" ]] || fail "auth config does not advertise local: $cfg"
  [[ "$(printf '%s' "$cfg" | jq -r '.registration_enabled')" == "false" ]] || fail "auth config advertises registration: $cfg"
  [[ "$(printf '%s' "$cfg" | jq -r '.enabled')" == "false" ]] || fail "auth config advertises OIDC it does not have: $cfg"
  pass "GET /v1/auth/config reports local auth with registration closed"

  # --- self-registration is refused ------------------------------------------
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$APP_BASE_URL/v1/auth/register" \
    -H 'Content-Type: application/json' -d '{"name":"Nope","email":"nope@example.test","password":"Password-1"}')
  [[ "$code" == "403" ]] || fail "POST /v1/auth/register closed -> $code, want 403"
  pass "closed registration is 403"

  # --- the API token bootstraps the first admin -------------------------------
  created=$(curl -fsS -X POST "$APP_BASE_URL/v1/users" \
    -H "X-Api-Token: $BEELINEWF_API_TOKEN" -H 'Content-Type: application/json' \
    -d '{"name":"Closed Admin","email":"closed-admin@example.test","password":"Password-1","roles":["admin"]}') \
    || fail "bootstrap the first admin"
  ADMIN_ID=$(printf '%s' "$created" | jq -er '.id')
  [[ "$(printf '%s' "$created" | jq -r '.is_local')" == "true" ]] || fail "created user is not local: $created"
  [[ "$(printf '%s' "$created" | jq -r 'has("password_hash")')" == "false" ]] || fail "created user leaks password_hash: $created"
  pass "API token bootstrapped admin $ADMIN_ID with no credential leak"

  # --- login returns a JWT that passes a role-gated route ----------------------
  login=$(curl -fsS -X POST "$APP_BASE_URL/v1/auth/login" \
    -H 'Content-Type: application/json' -d '{"email":"closed-admin@example.test","password":"Password-1"}') \
    || fail "login as the bootstrapped admin"
  TOKEN=$(printf '%s' "$login" | jq -er '.access_token')
  [[ "$(printf '%s' "$login" | jq -r '.token_type')" == "Bearer" ]] || fail "login token type: $login"
  me=$(curl -fsS "$APP_BASE_URL/v1/auth/me" -H "$(auth_header "$TOKEN")") || fail "auth me"
  [[ "$(printf '%s' "$me" | jq -er '.id')" == "$ADMIN_ID" ]] || fail "auth me id mismatch: $me"
  code=$(curl -sS -o /dev/null -w '%{http_code}' "$APP_BASE_URL/v1/roles" -H "$(auth_header "$TOKEN")")
  [[ "$code" == "200" ]] || fail "GET /v1/roles as admin -> $code, want 200"
  pass "login JWT resolves and passes a role-gated route"

  # --- a wrong password is 401 --------------------------------------------------
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$APP_BASE_URL/v1/auth/login" \
    -H 'Content-Type: application/json' -d '{"email":"closed-admin@example.test","password":"wrong"}')
  [[ "$code" == "401" ]] || fail "wrong password -> $code, want 401"
  pass "wrong password is 401"

  # --- deleting the user revokes the JWT ---------------------------------------
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$APP_BASE_URL/v1/users/$ADMIN_ID" \
    -H "X-Api-Token: $BEELINEWF_API_TOKEN")
  [[ "$code" == "204" ]] || fail "DELETE /v1/users -> $code, want 204"
  code=$(curl -sS -o /dev/null -w '%{http_code}' "$APP_BASE_URL/v1/auth/me" -H "$(auth_header "$TOKEN")")
  [[ "$code" == "401" ]] || fail "revoked JWT -> $code, want 401"
  pass "disabling the account revokes its JWT"

  echo "e2e-local: ALL CLOSED CHECKS PASSED"
  exit 0
fi

[[ "$MODE" == "open" ]] || fail "E2E_LOCAL_MODE must be closed or open, got $MODE"
[[ -n "${APP_LOG:-}" && -f "$APP_LOG" ]] \
  || fail "APP_LOG must point at the app log file: the verification link is lifted from it"

# --- the login contract reports registration with verification ----------------
cfg=$(curl -fsS "$APP_BASE_URL/v1/auth/config") || fail "read auth config without a credential"
[[ "$(printf '%s' "$cfg" | jq -r '.local_enabled')" == "true" ]] || fail "auth config does not advertise local: $cfg"
[[ "$(printf '%s' "$cfg" | jq -r '.registration_enabled')" == "true" ]] || fail "auth config does not advertise registration: $cfg"
[[ "$(printf '%s' "$cfg" | jq -r '.email_verification_enabled')" == "true" ]] || fail "auth config does not advertise verification: $cfg"
[[ "$(printf '%s' "$cfg" | jq -r '.enabled')" == "false" ]] || fail "auth config advertises OIDC it does not have: $cfg"
pass "GET /v1/auth/config reports local auth with verification"

# --- registration pends with no token -------------------------------------------
EMAIL="open-user@example.test"
reg=$(curl -fsS -X POST "$APP_BASE_URL/v1/auth/register" \
  -H 'Content-Type: application/json' -d "{\"name\":\"Open User\",\"email\":\"$EMAIL\",\"password\":\"Password-1\"}") \
  || fail "register"
[[ "$(printf '%s' "$reg" | jq -r '.pending_verification')" == "true" ]] || fail "registration is not pending: $reg"
[[ "$(printf '%s' "$reg" | jq -r 'has("access_token")')" == "false" ]] || fail "pending registration issued a token: $reg"
USER_ID=$(printf '%s' "$reg" | jq -er '.user.id')
pass "registration pends with no token ($USER_ID)"

# --- login before verification is 403, a wrong password is 401 -------------------
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$APP_BASE_URL/v1/auth/login" \
  -H 'Content-Type: application/json' -d "{\"email\":\"$EMAIL\",\"password\":\"Password-1\"}")
[[ "$code" == "403" ]] || fail "pending login -> $code, want 403"
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$APP_BASE_URL/v1/auth/login" \
  -H 'Content-Type: application/json' -d "{\"email\":\"$EMAIL\",\"password\":\"wrong\"}")
[[ "$code" == "401" ]] || fail "wrong password -> $code, want 401"
pass "pending login is 403 and a wrong password is 401"

# --- a garbage verification token is 422 -----------------------------------------
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$APP_BASE_URL/v1/auth/verify-email" \
  -H 'Content-Type: application/json' -d '{"token":"not-a-real-token"}')
[[ "$code" == "422" ]] || fail "garbage token -> $code, want 422"
pass "garbage verification token is 422"

# --- resend is generic: unknown and pending read identically ----------------------
unknown_body=$(curl -fsS -X POST "$APP_BASE_URL/v1/auth/resend-verification" \
  -H 'Content-Type: application/json' -d '{"email":"nobody@example.test"}') || fail "resend unknown"
pending_body=$(curl -fsS -X POST "$APP_BASE_URL/v1/auth/resend-verification" \
  -H 'Content-Type: application/json' -d "{\"email\":\"$EMAIL\"}") || fail "resend pending"
[[ "$unknown_body" == "$pending_body" ]] || fail "resend bodies differ: unknown=$unknown_body pending=$pending_body"
pass "resend is generic across unknown and pending emails"

# --- the logged link verifies the account and returns a session -------------------
link=""
for _ in $(seq 1 10); do
  # The log sender's line quotes the URL (link="http://..."), so the quotes
  # come off: with them the token would escape the JSON string and the
  # verify would be a misleading 400 instead of exercising the endpoint.
  link=$(grep 'verification link' "$APP_LOG" | grep "$EMAIL" | tail -1 | grep -o 'link=[^ ]*' | cut -d= -f2- | tr -d '"' || true)
  [[ -n "$link" ]] && break
  sleep 0.5
done
[[ -n "$link" ]] || fail "no verification link logged for $EMAIL"
raw="${link##*token=}"
[[ -n "$raw" && "$raw" != "$link" ]] || fail "logged link carries no token: $link"
verified=$(curl -fsS -X POST "$APP_BASE_URL/v1/auth/verify-email" \
  -H 'Content-Type: application/json' -d "{\"token\":\"$raw\"}") || fail "verify the mailed token"
TOKEN=$(printf '%s' "$verified" | jq -er '.access_token')
[[ "$(printf '%s' "$verified" | jq -er '.user.id')" == "$USER_ID" ]] || fail "verified user mismatch: $verified"
me=$(curl -fsS "$APP_BASE_URL/v1/auth/me" -H "$(auth_header "$TOKEN")") || fail "auth me after verify"
[[ "$(printf '%s' "$me" | jq -er '.id')" == "$USER_ID" ]] || fail "auth me id mismatch: $me"
pass "logged link verifies the account and returns a working JWT"

# --- successful logins never spend the throttle ------------------------------------
for _ in $(seq 1 5); do
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$APP_BASE_URL/v1/auth/login" \
    -H 'Content-Type: application/json' -d "{\"email\":\"$EMAIL\",\"password\":\"Password-1\"}")
  [[ "$code" == "200" ]] || fail "success burst -> $code, want 200"
done
pass "successful logins never spend the throttle"

# --- a duplicate registration is 409 ----------------------------------------------
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$APP_BASE_URL/v1/auth/register" \
  -H 'Content-Type: application/json' -d "{\"name\":\"Open Again\",\"email\":\"$EMAIL\",\"password\":\"Password-2\"}")
[[ "$code" == "409" ]] || fail "duplicate register -> $code, want 409"
pass "duplicate registration is 409"

# --- flooding login trips 429 with a Retry-After ------------------------------------
limited=0
for _ in $(seq 1 15); do
  headers=$(curl -sS -D - -o /dev/null -X POST "$APP_BASE_URL/v1/auth/login" \
    -H 'Content-Type: application/json' -d '{"email":"flood@example.test","password":"wrong"}')
  code=$(printf '%s' "$headers" | head -1 | awk '{print $2}')
  if [[ "$code" == "429" ]]; then
    limited=1
    printf '%s' "$headers" | grep -qi 'retry-after:' || fail "429 without a Retry-After"
    break
  fi
  [[ "$code" == "401" ]] || fail "flood attempt -> $code, want 401 until the limit"
done
[[ "$limited" == "1" ]] || fail "15 login attempts never tripped 429"
pass "flooding login trips 429 with a Retry-After"

echo "e2e-local: ALL OPEN CHECKS PASSED"

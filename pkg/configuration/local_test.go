package configuration_test

import (
	"strings"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/pkg/configuration"
)

// localFile is the minimal auth.local block tests build on: enabled with a
// valid secret so auth.enabled is satisfied without an API token.
const localFile = `
infra:
  postgresql:
    dsn: "file-dsn"
auth:
  enabled: true
  local:
    enabled: true
    jwt_secret: "0123456789abcdef0123456789abcdef"
`

func TestLoadLocalDefaults(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	local := cfg.Auth.Local
	if local.Enabled {
		t.Error("auth.local.enabled = true, want false by default")
	}
	if local.TokenTTL != 24*time.Hour {
		t.Errorf("auth.local.token_ttl = %v, want 24h", local.TokenTTL)
	}
	if local.Issuer != "beelinewf-local" {
		t.Errorf("auth.local.issuer = %q, want beelinewf-local", local.Issuer)
	}
	if local.ClientID != "beelinewf" {
		t.Errorf("auth.local.client_id = %q, want beelinewf", local.ClientID)
	}
	if local.AllowRegistration {
		t.Error("auth.local.allow_registration = true, want false by default")
	}
	if local.EmailVerification.Enabled {
		t.Error("auth.local.email_verification.enabled = true, want false by default")
	}
	if local.EmailVerification.TokenTTL != 24*time.Hour {
		t.Errorf("auth.local.email_verification.token_ttl = %v, want 24h", local.EmailVerification.TokenTTL)
	}
	if local.EmailVerification.ResendCooldown != time.Minute {
		t.Errorf("auth.local.email_verification.resend_cooldown = %v, want 60s", local.EmailVerification.ResendCooldown)
	}
	if local.SMTP.Port != 587 || !local.SMTP.UseTLS {
		t.Errorf("smtp port/use_tls = %d/%v, want 587/true", local.SMTP.Port, local.SMTP.UseTLS)
	}
	rl := local.RateLimit
	if !rl.Enabled || rl.MaxAttempts != 10 || rl.Window != time.Minute || rl.BlockDuration != 5*time.Minute {
		t.Errorf("rate_limit = %+v, want enabled 10/1m/5m", rl)
	}
}

// Every auth.local key resolves from the environment without a config file.
func TestLoadLocalEnvBinds(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_AUTH_ENABLED", "true")
	setenv(t, "BEELINEWF_AUTH_LOCAL_ENABLED", "true")
	setenv(t, "BEELINEWF_AUTH_LOCAL_JWT_SECRET", "0123456789abcdef0123456789abcdef")
	setenv(t, "BEELINEWF_AUTH_LOCAL_TOKEN_TTL", "2h")
	setenv(t, "BEELINEWF_AUTH_LOCAL_ISSUER", "custom-issuer")
	setenv(t, "BEELINEWF_AUTH_LOCAL_CLIENT_ID", "custom-client")
	setenv(t, "BEELINEWF_AUTH_LOCAL_ALLOW_REGISTRATION", "true")
	setenv(t, "BEELINEWF_AUTH_LOCAL_DEFAULT_ROLE", "viewer")
	setenv(t, "BEELINEWF_AUTH_LOCAL_PUBLIC_BASE_URL", "https://app.example.com")
	setenv(t, "BEELINEWF_AUTH_LOCAL_EMAIL_VERIFICATION_ENABLED", "true")
	setenv(t, "BEELINEWF_AUTH_LOCAL_EMAIL_VERIFICATION_TOKEN_TTL", "48h")
	setenv(t, "BEELINEWF_AUTH_LOCAL_EMAIL_VERIFICATION_RESEND_COOLDOWN", "90s")
	setenv(t, "BEELINEWF_AUTH_LOCAL_SMTP_HOST", "smtp.example.com")
	setenv(t, "BEELINEWF_AUTH_LOCAL_SMTP_PORT", "2525")
	setenv(t, "BEELINEWF_AUTH_LOCAL_SMTP_USERNAME", "user")
	setenv(t, "BEELINEWF_AUTH_LOCAL_SMTP_PASSWORD", "pass")
	setenv(t, "BEELINEWF_AUTH_LOCAL_SMTP_FROM", "noreply@example.com")
	setenv(t, "BEELINEWF_AUTH_LOCAL_SMTP_USE_TLS", "false")
	setenv(t, "BEELINEWF_AUTH_LOCAL_SMTP_TEMPLATE_DIR", "/tmp/mail-templates")
	setenv(t, "BEELINEWF_AUTH_LOCAL_RATE_LIMIT_ENABLED", "true")
	setenv(t, "BEELINEWF_AUTH_LOCAL_RATE_LIMIT_MAX_ATTEMPTS", "5")
	setenv(t, "BEELINEWF_AUTH_LOCAL_RATE_LIMIT_WINDOW", "2m")
	setenv(t, "BEELINEWF_AUTH_LOCAL_RATE_LIMIT_BLOCK_DURATION", "10m")

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	local := cfg.Auth.Local
	if !local.Enabled || local.TokenTTL != 2*time.Hour {
		t.Errorf("local enabled/token_ttl = %v/%v, want true/2h", local.Enabled, local.TokenTTL)
	}
	if local.Issuer != "custom-issuer" || local.ClientID != "custom-client" {
		t.Errorf("issuer/client_id = %q/%q, want custom values", local.Issuer, local.ClientID)
	}
	if !local.AllowRegistration || local.DefaultRole != "viewer" {
		t.Errorf("allow_registration/default_role = %v/%q, want true/viewer", local.AllowRegistration, local.DefaultRole)
	}
	if local.PublicBaseURL != "https://app.example.com" {
		t.Errorf("public_base_url = %q", local.PublicBaseURL)
	}
	ev := local.EmailVerification
	if !ev.Enabled || ev.TokenTTL != 48*time.Hour || ev.ResendCooldown != 90*time.Second {
		t.Errorf("email_verification = %+v, want enabled 48h/90s", ev)
	}
	smtp := local.SMTP
	if smtp.Host != "smtp.example.com" || smtp.Port != 2525 || smtp.Username != "user" ||
		smtp.Password != "pass" || smtp.From != "noreply@example.com" || smtp.UseTLS ||
		smtp.TemplateDir != "/tmp/mail-templates" {
		t.Errorf("smtp = %+v, want env values with use_tls false", smtp)
	}
	rl := local.RateLimit
	if !rl.Enabled || rl.MaxAttempts != 5 || rl.Window != 2*time.Minute || rl.BlockDuration != 10*time.Minute {
		t.Errorf("rate_limit = %+v, want 5/2m/10m", rl)
	}
}

// Env wins over the file for local keys: the e2e harness flips
// registration and verification per run through the environment.
func TestLoadLocalEnvOverridesFile(t *testing.T) {
	setenv(t, "BEELINEWF_AUTH_LOCAL_ALLOW_REGISTRATION", "false")
	setenv(t, "BEELINEWF_AUTH_LOCAL_EMAIL_VERIFICATION_ENABLED", "false")
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
auth:
  enabled: true
  local:
    enabled: true
    jwt_secret: "0123456789abcdef0123456789abcdef"
    allow_registration: true
    email_verification:
      enabled: true
`)
	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Auth.Local.AllowRegistration {
		t.Error("allow_registration = true, want env false to win over file true")
	}
	if cfg.Auth.Local.EmailVerification.Enabled {
		t.Error("email_verification.enabled = true, want env false to win over file true")
	}
}

func TestLoadLocalRequiresSecret(t *testing.T) {
	for _, secret := range []string{"", "short"} {
		path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
auth:
  enabled: true
  local:
    enabled: true
    jwt_secret: "`+secret+`"
`)
		if _, err := configuration.Load(configuration.WithConfigFile(path)); err == nil {
			t.Errorf("Load() with jwt_secret %q: error = nil, want error", secret)
		}
	}
}

// Disabled local auth ignores stray local settings instead of tripping
// validation.
func TestLoadLocalDisabledIsInert(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	// A short secret would fail validation if local auth were on.
	setenv(t, "BEELINEWF_AUTH_LOCAL_JWT_SECRET", "short")

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Auth.Local.Enabled {
		t.Error("auth.local.enabled = true, want false")
	}
}

// The master switch forces local auth (and verification) off, so a stray
// auth.local.enabled cannot protect /v1 or trip validation.
func TestLoadMasterSwitchForcesLocalOff(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
auth:
  enabled: false
  local:
    enabled: true
    email_verification:
      enabled: true
`)
	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Auth.Local.Enabled {
		t.Error("auth.local.enabled = true, want forced off by auth.enabled=false")
	}
	if cfg.Auth.Local.EmailVerification.Enabled {
		t.Error("email_verification.enabled = true, want forced off")
	}
}

func TestLoadVerificationRequiresSMTPAndBaseURL(t *testing.T) {
	cases := map[string]string{
		"missing smtp and base": `
  local:
    enabled: true
    jwt_secret: "0123456789abcdef0123456789abcdef"
    email_verification:
      enabled: true
`,
		"missing from": `
  local:
    enabled: true
    jwt_secret: "0123456789abcdef0123456789abcdef"
    public_base_url: "https://app.example.com"
    email_verification:
      enabled: true
    smtp:
      host: "smtp.example.com"
`,
	}
	for name, local := range cases {
		path := writeConfig(t, "infra:\n  postgresql:\n    dsn: \"file-dsn\"\nauth:\n  enabled: true\n"+local)
		if _, err := configuration.Load(configuration.WithConfigFile(path)); err == nil {
			t.Errorf("%s: Load() error = nil, want error", name)
		}
	}
}

// The verification link is mailed to strangers, so the base must be an
// absolute web origin: a bare host would join into a relative path and a
// javascript: scheme into an XSS payload.
func TestLoadPublicBaseURLRequiresHTTPScheme(t *testing.T) {
	verifying := func(base string) string {
		return `
infra:
  postgresql:
    dsn: "file-dsn"
auth:
  enabled: true
  local:
    enabled: true
    jwt_secret: "0123456789abcdef0123456789abcdef"
    public_base_url: "` + base + `"
    email_verification:
      enabled: true
    smtp:
      host: "log"
`
	}
	for _, base := range []string{"app.example.com", "javascript:alert(1)", "ftp://app.example.com", "https://"} {
		path := writeConfig(t, verifying(base))
		if _, err := configuration.Load(configuration.WithConfigFile(path)); err == nil {
			t.Errorf("base %q: Load() error = nil, want error", base)
		}
	}
	for _, base := range []string{"https://app.example.com", "http://localhost:3000/app"} {
		path := writeConfig(t, verifying(base))
		if _, err := configuration.Load(configuration.WithConfigFile(path)); err != nil {
			t.Errorf("base %q: Load() error = %v, want nil", base, err)
		}
	}
}

// The chain verifier routes on the unverified iss claim with local
// checked first, so a shared issuer would hand OIDC tokens to the local
// key: boot refuses the collision.
func TestLoadLocalIssuerMustDifferFromOIDC(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
auth:
  enabled: true
  oidc:
    enabled: true
    issuer: "beelinewf-local"
    client_id: "beelinewf"
  local:
    enabled: true
    jwt_secret: "0123456789abcdef0123456789abcdef"
    issuer: "beelinewf-local"
`)
	if _, err := configuration.Load(configuration.WithConfigFile(path)); err == nil {
		t.Fatal("Load() error = nil, want the issuer-collision error")
	}
}

// The log sender needs no from address: it exists for dev/e2e runs.
func TestLoadVerificationLogSenderNeedsNoFrom(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
auth:
  enabled: true
  local:
    enabled: true
    jwt_secret: "0123456789abcdef0123456789abcdef"
    public_base_url: "https://app.example.com"
    email_verification:
      enabled: true
    smtp:
      host: "log"
`)
	if _, err := configuration.Load(configuration.WithConfigFile(path)); err != nil {
		t.Fatalf("Load() error = %v, want nil for the log sender", err)
	}
}

func TestLoadLocalRateLimitInvalid(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
auth:
  enabled: true
  local:
    enabled: true
    jwt_secret: "0123456789abcdef0123456789abcdef"
    rate_limit:
      enabled: true
      max_attempts: 0
`)
	if _, err := configuration.Load(configuration.WithConfigFile(path)); err == nil {
		t.Fatal("Load() error = nil, want error for max_attempts 0")
	}
}

func TestLoadAuthEnabledRequiresACredential(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
auth:
  enabled: true
`)
	_, err := configuration.Load(configuration.WithConfigFile(path))
	if err == nil || !strings.Contains(err.Error(), "at least one credential") {
		t.Fatalf("Load() error = %v, want the at-least-one-credential error", err)
	}
}

// Local auth alone satisfies the master switch: no API token needed.
func TestLoadAuthEnabledLocalOnly(t *testing.T) {
	path := writeConfig(t, localFile)
	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Auth.Local.Enabled {
		t.Error("auth.local.enabled = false, want true")
	}
}

// OIDC alone also satisfies the master switch now: an OIDC deployment no
// longer needs a service token it never uses.
func TestLoadAuthEnabledOIDCOnly(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
auth:
  enabled: true
  oidc:
    enabled: true
    issuer: "https://issuer.example.com"
    client_id: "beelinewf"
`)
	if _, err := configuration.Load(configuration.WithConfigFile(path)); err != nil {
		t.Fatalf("Load() error = %v, want nil for OIDC without an API token", err)
	}
}

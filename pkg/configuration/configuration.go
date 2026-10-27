// Package configuration loads service configuration from a YAML config file
// and environment variables, merged per key.
//
// Precedence (highest first): environment variables > config file > defaults.
// Every key can be set as a BEELINEWF_-prefixed environment variable (dots in
// keys become underscores, e.g. BEELINEWF_WORKER_POOL_SIZE); a key absent from
// the environment falls back to the config file, then to the code default.
// A set-but-empty variable is treated as unset and falls back the same way.
// The role catalog is the exception: maps cannot ride AutomaticEnv, so
// auth.role_permissions is applied from BEELINEWF_AUTH_ROLE_PERMISSIONS as a
// post-unmarshal JSON override, where a set non-blank value wins over the
// file and blank falls back to it.
package configuration

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/beelinewf/wf-engine/pkg/ids"
	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

const (
	envPrefix         = "BEELINEWF"
	defaultConfigFile = "config.yaml"
)

// Config is the root configuration for the service.
type Config struct {
	Infra     Infra     `mapstructure:"infra"`
	Worker    Worker    `mapstructure:"worker"`
	Scheduler Scheduler `mapstructure:"scheduler"`
	Engine    Engine    `mapstructure:"engine"`
	System    System    `mapstructure:"system"`
	Auth      Auth      `mapstructure:"auth"`
}

// Infra groups infrastructure endpoints.
type Infra struct {
	HTTP       HTTP       `mapstructure:"http"`
	PostgreSQL PostgreSQL `mapstructure:"postgresql"`
	Redis      Redis      `mapstructure:"redis"`
	RabbitMQ   RabbitMQ   `mapstructure:"rabbitmq"`
	Storage    Storage    `mapstructure:"storage"`
}

// HTTP holds HTTP server settings.
type HTTP struct {
	Host           string `mapstructure:"host"`
	SwaggerEnabled bool   `mapstructure:"swagger_enabled"`
	MetricsEnabled bool   `mapstructure:"metrics_enabled"`
	// MaxBodyBytes caps the JSON request bodies the instance handler
	// reads (context replace, input delivery); a breach answers 413.
	MaxBodyBytes int64 `mapstructure:"max_body_bytes"`
	// ReadHeaderTimeout bounds the time the server waits for request
	// headers, closing slow-header connections. WriteTimeout stays unset
	// so long-lived SSE streams are never cut.
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout"`
	// IdleTimeout closes idle keep-alive connections for cleanup.
	IdleTimeout time.Duration `mapstructure:"idle_timeout"`
	CORS        CORS          `mapstructure:"cors"`
}

// CORS holds the cross-origin resource sharing settings. An empty
// AllowedOrigins disables CORS: the engine emits no Access-Control-*
// headers and every other field is ignored. The default is ["*"], which
// allows any origin for development; restrict it in production.
type CORS struct {
	// AllowedOrigins lists the origins a browser may read API responses
	// from, as scheme://host[:port] entries. A lone "*" allows any
	// origin, but only with AllowCredentials off.
	AllowedOrigins []string `mapstructure:"allowed_origins"`
	// AllowedMethods lists the request methods a preflight may approve.
	AllowedMethods []string `mapstructure:"allowed_methods"`
	// AllowedHeaders lists the request headers a preflight may approve.
	AllowedHeaders []string `mapstructure:"allowed_headers"`
	// ExposedHeaders lists the response headers a browser may read.
	ExposedHeaders []string `mapstructure:"exposed_headers"`
	// AllowCredentials permits credentialed requests. It requires
	// explicit origins: a credentialed response must name the origin,
	// never the "*" wildcard.
	AllowCredentials bool `mapstructure:"allow_credentials"`
	// MaxAge is how long a browser may cache a preflight response.
	MaxAge time.Duration `mapstructure:"max_age"`
}

// PostgreSQL holds the DSN and the connection pool ceiling for the
// application database.
type PostgreSQL struct {
	DSN             string        `mapstructure:"dsn"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
	ConnMaxIdleTime time.Duration `mapstructure:"conn_max_idle_time"`
}

// Redis holds the optional Redis connection. An empty DSN disables the
// Redis input/output/status transports; a configured but unreachable DSN
// fails startup.
type Redis struct {
	DSN string `mapstructure:"dsn"`
}

// RabbitMQ holds the optional RabbitMQ connection and the three durable
// queues used for input, output, and status messages. An empty DSN disables
// the RabbitMQ transports; a configured but unreachable DSN fails startup.
type RabbitMQ struct {
	DSN         string `mapstructure:"dsn"`
	InputQueue  string `mapstructure:"input_queue"`
	OutputQueue string `mapstructure:"output_queue"`
	StatusQueue string `mapstructure:"status_queue"`
}

// Storage holds the S3-compatible object storage connection serving
// multipart input uploads. Input nodes name only a bucket, an optional
// prefix, and optional file constraints; the endpoint and credentials live
// here so node definitions never carry them. Fully empty means storage is
// disabled: definitions still parse, but a multipart delivery fails at
// delivery time. A partial configuration fails startup.
type Storage struct {
	// Endpoint is the bare host:port of the S3 API (an http(s):// prefix
	// is tolerated).
	Endpoint string `mapstructure:"endpoint"`
	Region   string `mapstructure:"region"`
	UseSSL   bool   `mapstructure:"use_ssl"`
	// AccessKey and SecretKey are the static credentials.
	AccessKey string `mapstructure:"access_key"`
	SecretKey string `mapstructure:"secret_key"`
}

// Enabled reports whether object storage is configured. Partial
// configurations are rejected at load, so any single set field implies a
// complete one.
func (s Storage) Enabled() bool {
	return s.Endpoint != "" || s.AccessKey != "" || s.SecretKey != ""
}

// Worker holds the ants worker pool configuration.
type Worker struct {
	// Pool sizes the engine dispatcher; StatusPool sizes the
	// status-update dispatcher. ants internals stay on library defaults.
	Pool       WorkerPool `mapstructure:"pool"`
	StatusPool WorkerPool `mapstructure:"status_pool"`
}

// WorkerPool holds the pool size.
type WorkerPool struct {
	Size int `mapstructure:"size"`
}

// Scheduler holds the cron-scheduler firing-loop settings.
type Scheduler struct {
	// Enabled starts the tick firing loop. Every enabled replica fires
	// the same ticks, but the fire claim keeps each tick to one
	// instance; disable on replicas that should serve API only.
	Enabled bool `mapstructure:"enabled"`
	// RefreshInterval reloads the enabled schedules from the database so
	// mutations made on other replicas take effect locally.
	RefreshInterval time.Duration `mapstructure:"refresh_interval"`
}

// Engine holds the workflow engine limits.
type Engine struct {
	DefaultNodeTimeout   time.Duration `mapstructure:"default_node_timeout"`
	MaxNodeTimeout       time.Duration `mapstructure:"max_node_timeout"`
	ConditionTimeout     time.Duration `mapstructure:"condition_timeout"`
	MaxPerNodeExecutions int           `mapstructure:"max_per_node_executions"`
	MaxTotalExecutions   int           `mapstructure:"max_total_executions"`
	LeaseDuration        time.Duration `mapstructure:"lease_duration"`
	ClaimBatchSize       int           `mapstructure:"claim_batch_size"`
	// PollInterval paces both dispatchers' claim loops; HeartbeatInterval
	// paces the engine dispatcher's lease-renewal loop (the status
	// dispatcher has no heartbeat loop).
	PollInterval           time.Duration `mapstructure:"poll_interval"`
	HeartbeatInterval      time.Duration `mapstructure:"heartbeat_interval"`
	MaxOutputBytes         int           `mapstructure:"max_output_bytes"`
	MaxRedirects           int           `mapstructure:"max_redirects"`
	HTTPMaxInFlight        int           `mapstructure:"http_max_in_flight"`
	HTTPMaxInFlightPerHost int           `mapstructure:"http_max_in_flight_per_host"`
	HTTPAllowlist          []string      `mapstructure:"http_allowlist"`
	ExecAllowlist          []string      `mapstructure:"exec_allowlist"`
	LeanContextDefault     bool          `mapstructure:"lean_context_default"`
	LeanAnchorEvery        int           `mapstructure:"lean_anchor_every"`
	LeanReplayMax          int           `mapstructure:"lean_replay_max"`
	// EnvDenyExtra and EnvAllowExceptions adjust the hardcoded env snapshot
	// deny list: extra adds patterns, allow narrows it with exceptions. They
	// never replace the defaults, so a misconfiguration cannot silently open
	// the whole namespace.
	EnvDenyExtra       []string `mapstructure:"env_deny_extra"`
	EnvAllowExceptions []string `mapstructure:"env_allow_exceptions"`
	// Parallel bounds parallel execution fan-out, nesting, and load.
	Parallel Parallel `mapstructure:"parallel"`
	// ConsumerRetry bounds broker input consumer supervision.
	ConsumerRetry ConsumerRetry `mapstructure:"consumer_retry"`
	// Multipart bounds streaming multipart input uploads.
	Multipart Multipart `mapstructure:"multipart"`
}

// Parallel holds the parallel execution safety limits.
type Parallel struct {
	// MaxDepth caps structural parallel nesting (1 = one parallel block).
	MaxDepth int `mapstructure:"max_depth"`
	// MaxBranchesPerParallel caps the branch count of one parallel_start.
	MaxBranchesPerParallel int `mapstructure:"max_branches_per_parallel"`
	// MaxActiveBranchesPerInstance caps concurrently active branches of one
	// instance across all its parallel executions.
	MaxActiveBranchesPerInstance int `mapstructure:"max_active_branches_per_instance"`
}

// ConsumerRetry holds the broker input consumer supervision budget. A
// consumer that dies is retried with doubling backoff (InitialBackoff to
// MaxBackoff, plus jitter) up to MaxAttempts consecutive failures, then
// marked down until restart while /ready reports 503. An attempt living
// past MinHealthyRun resets the consecutive-failure count.
type ConsumerRetry struct {
	MaxAttempts    int           `mapstructure:"max_attempts"`
	InitialBackoff time.Duration `mapstructure:"initial_backoff"`
	MaxBackoff     time.Duration `mapstructure:"max_backoff"`
	MinHealthyRun  time.Duration `mapstructure:"min_healthy_run"`
}

// Multipart holds the streaming multipart input upload limits. Every
// breach fails the delivery and deletes the objects the request already
// uploaded, so a failed delivery leaves nothing behind.
type Multipart struct {
	// MaxFiles caps the file-part count of one delivery.
	MaxFiles int `mapstructure:"max_files"`
	// MaxTotalBytes caps the combined file and text bytes of one delivery.
	MaxTotalBytes int64 `mapstructure:"max_total_bytes"`
	// MaxFileBytes caps one file when the input node sets no
	// max_file_bytes of its own.
	MaxFileBytes int64 `mapstructure:"max_file_bytes"`
	// MaxTextBytes caps one text part.
	MaxTextBytes int64 `mapstructure:"max_text_bytes"`
}

// System holds the configured audit actor (no auth yet).
type System struct {
	UserID string `mapstructure:"user_id"`
	Name   string `mapstructure:"name"`
	Email  string `mapstructure:"email"`
}

// Auth holds the authentication settings: the optional API token service
// principal and the optional OIDC resource-server settings.
type Auth struct {
	Enabled  bool   `mapstructure:"enabled"`
	APIToken string `mapstructure:"api_token"`
	// OIDC enables bearer-token authentication against any OIDC provider.
	// The engine keeps no sessions and no callback: the frontend runs
	// code+PKCE against the provider and calls the API with the JWT.
	OIDC OIDC `mapstructure:"oidc"`
	// RolePermissions is the static role catalog: role name to the
	// resource-action permissions it grants. At startup it upserts the
	// role and permission rows and prunes the permission rows of these
	// roles that are no longer granted; no role row is ever deleted.
	RolePermissions map[string][]string `mapstructure:"role_permissions"`
}

// OIDC holds the OIDC resource-server settings.
type OIDC struct {
	Enabled bool `mapstructure:"enabled"`
	// Issuer is the provider base URL used for discovery and for the
	// iss check.
	Issuer string `mapstructure:"issuer"`
	// ClientID is the expected aud value.
	ClientID string `mapstructure:"client_id"`
	// Audience overrides the expected aud value when the provider issues
	// an API audience distinct from the client id.
	Audience string `mapstructure:"audience"`
	// RolesClaim is the claim carrying the caller's roles. Default "roles".
	// A dotted path reads a nested claim, e.g. "realm_access.roles".
	RolesClaim string `mapstructure:"roles_claim"`
	// UsernameClaim overrides the claim used as the display name.
	// Default "name", falling back to "preferred_username" then email.
	// A dotted path reads a nested claim, e.g. "profile.display".
	UsernameClaim string `mapstructure:"username_claim"`
	// ClockSkew tolerates a small issuer/applicant clock difference on a
	// token's exp: a token that expired less than ClockSkew ago is retried
	// against a widened window. It is not applied to nbf, so a token issued
	// ahead of this clock is still rejected. Default 1m.
	ClockSkew time.Duration `mapstructure:"clock_skew"`
	// CacheTTL is how long a fetched discovery document and JWKS stay
	// fresh before they are refetched. Default 5m.
	CacheTTL time.Duration `mapstructure:"cache_ttl"`
}

// Option customizes Load behavior.
type Option func(*loader)

type loader struct {
	configFile string
}

// WithConfigFile points Load at an explicit config file. An empty path means
// the default "config.yaml" in the working directory.
func WithConfigFile(path string) Option {
	return func(l *loader) { l.configFile = path }
}

// Load builds a validated Config.
func Load(opts ...Option) (*Config, error) {
	l := loader{}
	for _, opt := range opts {
		opt(&l)
	}

	explicit := l.configFile != ""
	path := l.configFile
	if path == "" {
		path = defaultConfigFile
	}

	v := viper.New()
	setDefaults(v)
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	// auth.api_token is exposed as BEELINEWF_API_TOKEN (not
	// BEELINEWF_AUTH_API_TOKEN) to match the requested env contract.
	_ = v.BindEnv("auth.api_token", "BEELINEWF_API_TOKEN")
	// auth.enabled is bound explicitly because BEELINEWF_AUTH_ENABLED is the
	// switch compose and the e2e harness use to turn authentication on
	// without rewriting config.yaml.
	_ = v.BindEnv("auth.enabled", "BEELINEWF_AUTH_ENABLED")
	// auth.oidc.* is exposed as BEELINEWF_AUTH_OIDC_* (the default mapping),
	// which is bound explicitly so the keys resolve without a config file.
	for _, key := range []string{
		"enabled", "issuer", "client_id", "audience",
		"roles_claim", "username_claim", "clock_skew", "cache_ttl",
	} {
		_ = v.BindEnv("auth.oidc."+key, "BEELINEWF_AUTH_OIDC_"+strings.ToUpper(key))
	}
	// Lean settings are operator toggles and must override config.yaml.
	_ = v.BindEnv("engine.lean_context_default", "BEELINEWF_ENGINE_LEAN_CONTEXT_DEFAULT")
	_ = v.BindEnv("engine.lean_anchor_every", "BEELINEWF_ENGINE_LEAN_ANCHOR_EVERY")
	_ = v.BindEnv("engine.lean_replay_max", "BEELINEWF_ENGINE_LEAN_REPLAY_MAX")
	// Env snapshot deny adjustments are operator toggles and must override
	// config.yaml.
	_ = v.BindEnv("engine.env_deny_extra", "BEELINEWF_ENGINE_ENV_DENY_EXTRA")
	_ = v.BindEnv("engine.env_allow_exceptions", "BEELINEWF_ENGINE_ENV_ALLOW_EXCEPTIONS")
	// Consumer retry knobs are operator toggles and must override
	// config.yaml.
	_ = v.BindEnv("engine.consumer_retry.max_attempts", "BEELINEWF_ENGINE_CONSUMER_RETRY_MAX_ATTEMPTS")
	_ = v.BindEnv("engine.consumer_retry.initial_backoff", "BEELINEWF_ENGINE_CONSUMER_RETRY_INITIAL_BACKOFF")
	_ = v.BindEnv("engine.consumer_retry.max_backoff", "BEELINEWF_ENGINE_CONSUMER_RETRY_MAX_BACKOFF")
	_ = v.BindEnv("engine.consumer_retry.min_healthy_run", "BEELINEWF_ENGINE_CONSUMER_RETRY_MIN_HEALTHY_RUN")
	// HTTP bulkhead caps are operator toggles and must override
	// config.yaml.
	_ = v.BindEnv("engine.http_max_in_flight", "BEELINEWF_ENGINE_HTTP_MAX_IN_FLIGHT")
	_ = v.BindEnv("engine.http_max_in_flight_per_host", "BEELINEWF_ENGINE_HTTP_MAX_IN_FLIGHT_PER_HOST")
	_ = v.BindEnv("infra.http.metrics_enabled", "BEELINEWF_INFRA_HTTP_METRICS_ENABLED")
	_ = v.BindEnv("infra.http.max_body_bytes", "BEELINEWF_INFRA_HTTP_MAX_BODY_BYTES")
	_ = v.BindEnv("infra.http.read_header_timeout", "BEELINEWF_INFRA_HTTP_READ_HEADER_TIMEOUT")
	_ = v.BindEnv("infra.http.idle_timeout", "BEELINEWF_INFRA_HTTP_IDLE_TIMEOUT")
	// CORS keys are operator toggles and must override the file, so a
	// frontend origin can be allowed without rewriting config.yaml.
	_ = v.BindEnv("infra.http.cors.allowed_origins", "BEELINEWF_INFRA_HTTP_CORS_ALLOWED_ORIGINS")
	_ = v.BindEnv("infra.http.cors.allowed_methods", "BEELINEWF_INFRA_HTTP_CORS_ALLOWED_METHODS")
	_ = v.BindEnv("infra.http.cors.allowed_headers", "BEELINEWF_INFRA_HTTP_CORS_ALLOWED_HEADERS")
	_ = v.BindEnv("infra.http.cors.exposed_headers", "BEELINEWF_INFRA_HTTP_CORS_EXPOSED_HEADERS")
	_ = v.BindEnv("infra.http.cors.allow_credentials", "BEELINEWF_INFRA_HTTP_CORS_ALLOW_CREDENTIALS")
	_ = v.BindEnv("infra.http.cors.max_age", "BEELINEWF_INFRA_HTTP_CORS_MAX_AGE")

	if _, err := os.Stat(path); err != nil {
		if explicit {
			return nil, fmt.Errorf("configuration: explicit config file %s: %w", path, err)
		}
		slog.Info("configuration: no config file, using environment and defaults", "path", path)
	} else {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("configuration: read config file %s: %w", path, err)
		}
		slog.Info("configuration: loaded config file", "path", path)
	}
	// auth.role_permissions cannot ride AutomaticEnv: a set-but-blank
	// BEELINEWF_AUTH_ROLE_PERMISSIONS would shadow the file subtree with an
	// empty catalog during unmarshal. When the JSON override below will
	// not run (unset or blank), pin the file value above the environment
	// so it survives.
	if strings.TrimSpace(os.Getenv("BEELINEWF_AUTH_ROLE_PERMISSIONS")) == "" {
		if fileVal := v.Get("auth.role_permissions"); fileVal != nil {
			v.Set("auth.role_permissions", fileVal)
		}
	}
	// AutomaticEnv runs whether or not a file was read, so every key
	// merges per key: environment > file > defaults.
	v.AutomaticEnv()

	var cfg Config
	if err := v.Unmarshal(&cfg, viper.DecodeHook(mapstructure.ComposeDecodeHookFunc(
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","),
	))); err != nil {
		return nil, fmt.Errorf("configuration: unmarshal: %w", err)
	}
	// auth.role_permissions is exposed as BEELINEWF_AUTH_ROLE_PERMISSIONS, a JSON
	// object of role name to action list. Applied as a post-unmarshal override
	// (not BindEnv): a map cannot ride AutomaticEnv, and an explicit override
	// gives env-wins precedence plus a fail-fast JSON error.
	if raw := strings.TrimSpace(os.Getenv("BEELINEWF_AUTH_ROLE_PERMISSIONS")); raw != "" {
		var catalog map[string][]string
		if err := json.Unmarshal([]byte(raw), &catalog); err != nil {
			return nil, fmt.Errorf("configuration: BEELINEWF_AUTH_ROLE_PERMISSIONS: invalid JSON: %w", err)
		}
		cfg.Auth.RolePermissions = catalog
	}
	// auth.enabled is the master switch: with it off, OIDC is forced off so
	// a stray auth.oidc.enabled cannot protect /v1 or trip OIDC validation.
	if !cfg.Auth.Enabled {
		cfg.Auth.OIDC.Enabled = false
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func setDefaults(v *viper.Viper) {
	// Register every env-addressable key so AutomaticEnv can find it; an empty
	// default keeps validation meaningful.
	v.SetDefault("auth.enabled", false)
	v.SetDefault("auth.api_token", "")
	// auth.role_permissions has no default: Load reads it from
	// BEELINEWF_AUTH_ROLE_PERMISSIONS as a post-unmarshal override. Registering
	// it here would put it in AllKeys and let AutomaticEnv hand the raw JSON
	// string to the map decoder in env-only mode. Omitting it leaves the
	// catalog nil until the file or the override supplies one, which
	// validate() already treats as "no catalog".
	// OIDC stays inert until enabled; the defaults keep every key
	// env-addressable so validation stays meaningful.
	v.SetDefault("auth.oidc.enabled", false)
	v.SetDefault("auth.oidc.issuer", "")
	v.SetDefault("auth.oidc.client_id", "")
	v.SetDefault("auth.oidc.audience", "")
	v.SetDefault("auth.oidc.roles_claim", "roles")
	v.SetDefault("auth.oidc.username_claim", "name")
	v.SetDefault("auth.oidc.clock_skew", "1m")
	v.SetDefault("auth.oidc.cache_ttl", "5m")
	v.SetDefault("infra.postgresql.dsn", "")
	v.SetDefault("infra.postgresql.max_open_conns", 25)
	v.SetDefault("infra.postgresql.max_idle_conns", 25)
	v.SetDefault("infra.postgresql.conn_max_lifetime", "5m")
	v.SetDefault("infra.postgresql.conn_max_idle_time", "5m")
	v.SetDefault("infra.http.host", "localhost:8080")
	v.SetDefault("infra.http.swagger_enabled", true)
	v.SetDefault("infra.http.metrics_enabled", true)
	v.SetDefault("infra.http.max_body_bytes", 1048576)
	v.SetDefault("infra.http.read_header_timeout", "10s")
	v.SetDefault("infra.http.idle_timeout", "60s")
	// CORS allows any origin by default, so a development frontend works
	// with no configuration. The method default covers every verb the
	// router registers; the header default covers the credentials the
	// API reads.
	v.SetDefault("infra.http.cors.allowed_origins", []string{"*"})
	v.SetDefault("infra.http.cors.allowed_methods", []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"})
	v.SetDefault("infra.http.cors.allowed_headers", []string{"Authorization", "Content-Type", "X-Api-Token"})
	v.SetDefault("infra.http.cors.exposed_headers", []string{})
	v.SetDefault("infra.http.cors.allow_credentials", false)
	v.SetDefault("infra.http.cors.max_age", "12h")
	// Optional brokers: empty DSN disables the transport. Queue names carry
	// safe defaults so an enabled RabbitMQ always has valid destinations.
	v.SetDefault("infra.redis.dsn", "")
	v.SetDefault("infra.rabbitmq.dsn", "")
	v.SetDefault("infra.rabbitmq.input_queue", "beelinewf.input")
	v.SetDefault("infra.rabbitmq.output_queue", "beelinewf.output")
	v.SetDefault("infra.rabbitmq.status_queue", "beelinewf.status")
	// Optional object storage for multipart input uploads: empty endpoint
	// and credentials disable it. Region and use_ssl carry safe defaults
	// so an enabled storage always has a valid connection shape.
	v.SetDefault("infra.storage.endpoint", "")
	v.SetDefault("infra.storage.region", "us-east-1")
	v.SetDefault("infra.storage.use_ssl", true)
	v.SetDefault("infra.storage.access_key", "")
	v.SetDefault("infra.storage.secret_key", "")
	v.SetDefault("worker.pool.size", 1000)
	v.SetDefault("worker.status_pool.size", 20)
	v.SetDefault("scheduler.enabled", true)
	v.SetDefault("scheduler.refresh_interval", "30s")
	v.SetDefault("engine.default_node_timeout", "30s")
	v.SetDefault("engine.max_node_timeout", "5m")
	v.SetDefault("engine.condition_timeout", "5s")
	// Zero/empty defaults register the keys so AutomaticEnv can address them;
	// optional limits keep their downstream fallback behavior.
	v.SetDefault("engine.max_per_node_executions", 0)
	v.SetDefault("engine.max_total_executions", 0)
	v.SetDefault("engine.lease_duration", "0s")
	v.SetDefault("engine.claim_batch_size", 0)
	v.SetDefault("engine.poll_interval", "200ms")
	v.SetDefault("engine.heartbeat_interval", "5s")
	v.SetDefault("engine.max_output_bytes", 0)
	v.SetDefault("engine.max_redirects", 0)
	v.SetDefault("engine.http_max_in_flight", 128)
	v.SetDefault("engine.http_max_in_flight_per_host", 16)
	v.SetDefault("engine.http_allowlist", []string{})
	v.SetDefault("engine.exec_allowlist", []string{})
	v.SetDefault("engine.lean_context_default", false)
	v.SetDefault("engine.lean_anchor_every", 20)
	v.SetDefault("engine.lean_replay_max", 500)
	v.SetDefault("engine.env_deny_extra", []string{})
	v.SetDefault("engine.env_allow_exceptions", []string{})
	v.SetDefault("engine.parallel.max_depth", 4)
	v.SetDefault("engine.parallel.max_branches_per_parallel", 32)
	v.SetDefault("engine.parallel.max_active_branches_per_instance", 128)
	v.SetDefault("engine.consumer_retry.max_attempts", 3)
	v.SetDefault("engine.consumer_retry.initial_backoff", "1s")
	v.SetDefault("engine.consumer_retry.max_backoff", "30s")
	v.SetDefault("engine.consumer_retry.min_healthy_run", "30s")
	v.SetDefault("engine.multipart.max_files", 20)
	v.SetDefault("engine.multipart.max_total_bytes", 134217728)
	v.SetDefault("engine.multipart.max_file_bytes", 33554432)
	v.SetDefault("engine.multipart.max_text_bytes", 1048576)
	v.SetDefault("system.user_id", "00000000-0000-7000-8000-000000000001")
	v.SetDefault("system.name", "system")
	v.SetDefault("system.email", "system@localhost")
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Infra.PostgreSQL.DSN) == "" {
		return errors.New("configuration: infra.postgresql.dsn is required")
	}
	if c.Infra.PostgreSQL.MaxOpenConns <= 0 {
		return errors.New("configuration: infra.postgresql.max_open_conns must be > 0")
	}
	if c.Infra.PostgreSQL.MaxIdleConns < 0 || c.Infra.PostgreSQL.MaxIdleConns > c.Infra.PostgreSQL.MaxOpenConns {
		return errors.New("configuration: infra.postgresql.max_idle_conns must be >= 0 and <= infra.postgresql.max_open_conns")
	}
	if c.Infra.PostgreSQL.ConnMaxLifetime < 0 {
		return errors.New("configuration: infra.postgresql.conn_max_lifetime must be >= 0")
	}
	if c.Infra.PostgreSQL.ConnMaxIdleTime < 0 {
		return errors.New("configuration: infra.postgresql.conn_max_idle_time must be >= 0")
	}
	if c.Infra.HTTP.MaxBodyBytes <= 0 {
		return errors.New("configuration: infra.http.max_body_bytes must be > 0")
	}
	if c.Infra.HTTP.ReadHeaderTimeout <= 0 {
		return errors.New("configuration: infra.http.read_header_timeout must be > 0")
	}
	if c.Infra.HTTP.IdleTimeout <= 0 {
		return errors.New("configuration: infra.http.idle_timeout must be > 0")
	}
	cors := c.Infra.HTTP.CORS
	wildcard := false
	for _, origin := range cors.AllowedOrigins {
		if strings.TrimSpace(origin) == "*" {
			wildcard = true
			continue
		}
		if !validCORSOrigin(origin) {
			return fmt.Errorf("configuration: infra.http.cors.allowed_origins: %q must be scheme://host[:port]", origin)
		}
	}
	if wildcard && len(cors.AllowedOrigins) > 1 {
		return errors.New("configuration: infra.http.cors.allowed_origins: \"*\" must be the only entry")
	}
	if wildcard && cors.AllowCredentials {
		return errors.New("configuration: infra.http.cors.allow_credentials requires explicit infra.http.cors.allowed_origins (\"*\" is forbidden)")
	}
	if cors.MaxAge < 0 {
		return errors.New("configuration: infra.http.cors.max_age must be >= 0")
	}
	if c.Worker.Pool.Size <= 0 {
		return errors.New("configuration: worker.pool.size must be > 0")
	}
	if c.Worker.StatusPool.Size <= 0 {
		return errors.New("configuration: worker.status_pool.size must be > 0")
	}
	if c.Scheduler.RefreshInterval <= 0 {
		return errors.New("configuration: scheduler.refresh_interval must be > 0")
	}
	if c.Engine.DefaultNodeTimeout <= 0 {
		return errors.New("configuration: engine.default_node_timeout must be > 0")
	}
	if c.Engine.MaxNodeTimeout < c.Engine.DefaultNodeTimeout {
		return errors.New("configuration: engine.max_node_timeout must be >= engine.default_node_timeout")
	}
	if c.Engine.ConditionTimeout <= 0 {
		return errors.New("configuration: engine.condition_timeout must be > 0")
	}
	if c.Engine.PollInterval <= 0 {
		return errors.New("configuration: engine.poll_interval must be > 0")
	}
	if c.Engine.HeartbeatInterval <= 0 {
		return errors.New("configuration: engine.heartbeat_interval must be > 0")
	}
	if c.Engine.HTTPMaxInFlight <= 0 {
		return errors.New("configuration: engine.http_max_in_flight must be > 0")
	}
	if c.Engine.HTTPMaxInFlightPerHost <= 0 {
		return errors.New("configuration: engine.http_max_in_flight_per_host must be > 0")
	}
	if c.Engine.LeanAnchorEvery <= 0 {
		return errors.New("configuration: engine.lean_anchor_every must be > 0")
	}
	if c.Engine.LeanReplayMax <= 0 {
		return errors.New("configuration: engine.lean_replay_max must be > 0")
	}
	if c.Engine.Parallel.MaxDepth <= 0 {
		return errors.New("configuration: engine.parallel.max_depth must be > 0")
	}
	if c.Engine.Parallel.MaxBranchesPerParallel <= 0 {
		return errors.New("configuration: engine.parallel.max_branches_per_parallel must be > 0")
	}
	if c.Engine.Parallel.MaxActiveBranchesPerInstance <= 0 {
		return errors.New("configuration: engine.parallel.max_active_branches_per_instance must be > 0")
	}
	if c.Engine.ConsumerRetry.MaxAttempts <= 0 {
		return errors.New("configuration: engine.consumer_retry.max_attempts must be > 0")
	}
	if c.Engine.ConsumerRetry.InitialBackoff <= 0 {
		return errors.New("configuration: engine.consumer_retry.initial_backoff must be > 0")
	}
	if c.Engine.ConsumerRetry.MaxBackoff < c.Engine.ConsumerRetry.InitialBackoff {
		return errors.New("configuration: engine.consumer_retry.max_backoff must be >= engine.consumer_retry.initial_backoff")
	}
	if c.Engine.ConsumerRetry.MinHealthyRun <= 0 {
		return errors.New("configuration: engine.consumer_retry.min_healthy_run must be > 0")
	}
	if err := validateEnvPatterns("engine.env_deny_extra", c.Engine.EnvDenyExtra); err != nil {
		return err
	}
	if err := validateEnvPatterns("engine.env_allow_exceptions", c.Engine.EnvAllowExceptions); err != nil {
		return err
	}
	if strings.TrimSpace(c.Infra.RabbitMQ.DSN) != "" {
		if strings.TrimSpace(c.Infra.RabbitMQ.InputQueue) == "" {
			return errors.New("configuration: infra.rabbitmq.input_queue is required when infra.rabbitmq.dsn is set")
		}
		if strings.TrimSpace(c.Infra.RabbitMQ.OutputQueue) == "" {
			return errors.New("configuration: infra.rabbitmq.output_queue is required when infra.rabbitmq.dsn is set")
		}
		if strings.TrimSpace(c.Infra.RabbitMQ.StatusQueue) == "" {
			return errors.New("configuration: infra.rabbitmq.status_queue is required when infra.rabbitmq.dsn is set")
		}
	}
	// Object storage is all or nothing: a partial connection (endpoint
	// without credentials, or credentials without an endpoint) fails the
	// boot instead of failing the first multipart delivery. Fully empty
	// disables multipart uploads; definitions still parse.
	if c.Infra.Storage.Enabled() {
		if strings.TrimSpace(c.Infra.Storage.Endpoint) == "" {
			return errors.New("configuration: infra.storage.endpoint is required when infra.storage credentials are set")
		}
		if strings.TrimSpace(c.Infra.Storage.AccessKey) == "" || strings.TrimSpace(c.Infra.Storage.SecretKey) == "" {
			return errors.New("configuration: infra.storage.access_key and infra.storage.secret_key are required when infra.storage.endpoint is set")
		}
	}
	if c.Engine.Multipart.MaxFiles <= 0 {
		return errors.New("configuration: engine.multipart.max_files must be > 0")
	}
	if c.Engine.Multipart.MaxTotalBytes <= 0 {
		return errors.New("configuration: engine.multipart.max_total_bytes must be > 0")
	}
	if c.Engine.Multipart.MaxFileBytes <= 0 {
		return errors.New("configuration: engine.multipart.max_file_bytes must be > 0")
	}
	if c.Engine.Multipart.MaxTextBytes <= 0 {
		return errors.New("configuration: engine.multipart.max_text_bytes must be > 0")
	}
	if c.Auth.Enabled && strings.TrimSpace(c.Auth.APIToken) == "" {
		return errors.New("configuration: auth.api_token is required when auth.enabled is true")
	}
	if c.Auth.OIDC.Enabled {
		if strings.TrimSpace(c.Auth.OIDC.Issuer) == "" {
			return errors.New("configuration: auth.oidc.issuer is required when auth.oidc.enabled is true")
		}
		if strings.TrimSpace(c.Auth.OIDC.ClientID) == "" {
			return errors.New("configuration: auth.oidc.client_id is required when auth.oidc.enabled is true")
		}
		if c.Auth.OIDC.ClockSkew < 0 {
			return errors.New("configuration: auth.oidc.clock_skew must be >= 0")
		}
		if c.Auth.OIDC.CacheTTL <= 0 {
			return errors.New("configuration: auth.oidc.cache_ttl must be > 0")
		}
	}
	for role := range c.Auth.RolePermissions {
		if strings.TrimSpace(role) == "" {
			return errors.New("configuration: auth.role_permissions keys must be non-empty")
		}
	}
	// The system user is the audit actor the API-token and broker bypasses
	// are recorded as, so every created_by/updated_by written on their behalf
	// points at this uuid. A value that is blank or is not a canonical uuid
	// fails the foreign key at the first write instead, by which point the
	// deployment has already served traffic attributed to nobody.
	if !ids.Valid(c.System.UserID) {
		return fmt.Errorf("configuration: system.user_id %q must be a canonical uuid", c.System.UserID)
	}
	return nil
}

// validCORSOrigin reports whether s is a bare origin: an http(s) URL with a
// host and no path, query, fragment, or userinfo. A trailing slash is
// tolerated because operators copy origins from a browser address bar.
func validCORSOrigin(s string) bool {
	trimmed := strings.TrimRight(strings.TrimSpace(s), "/")
	if trimmed == "" {
		return false
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return false
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	if u.Host == "" || u.User != nil {
		return false
	}
	return u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}

// validateEnvPatterns rejects a malformed env deny/allow pattern. A pattern
// that fails to compile would silently never match, leaving the operator's
// intent unenforced, so it fails the boot instead.
func validateEnvPatterns(field string, patterns []string) error {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, err := path.Match(p, "BEELINEWF_PROBE"); err != nil {
			return fmt.Errorf("configuration: %s: %w", field, err)
		}
	}
	return nil
}

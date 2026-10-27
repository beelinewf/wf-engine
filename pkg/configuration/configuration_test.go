package configuration_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/pkg/configuration"
	"github.com/beelinewf/wf-engine/pkg/ids"
)

const testDSN = "host=localhost user=gorm password=gorm dbname=gorm port=9921 sslmode=disable"

// missingPath returns a config file path that never exists.
func missingPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "does-not-exist.yaml")
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func setenv(t *testing.T, key, value string) {
	t.Helper()
	t.Setenv(key, value)
}

func TestLoadEnvOnly(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_INFRA_HTTP_HOST", "localhost:9999")
	setenv(t, "BEELINEWF_INFRA_HTTP_SWAGGER_ENABLED", "false")

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Infra.HTTP.Host != "localhost:9999" {
		t.Errorf("host = %q, want localhost:9999", cfg.Infra.HTTP.Host)
	}
	if cfg.Infra.PostgreSQL.DSN != testDSN {
		t.Errorf("dsn = %q, want %q", cfg.Infra.PostgreSQL.DSN, testDSN)
	}
	if cfg.Infra.HTTP.SwaggerEnabled {
		t.Error("swagger_enabled = true, want false from environment")
	}
}

func TestLoadEnvOverridesConfigFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  http:
    host: "file-host:1234"
    swagger_enabled: false
  postgresql:
    dsn: "file-dsn"
worker:
  pool:
    size: 4
`)
	setenv(t, "BEELINEWF_INFRA_HTTP_HOST", "env-host:5678")
	setenv(t, "BEELINEWF_WORKER_POOL_SIZE", "7")

	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Infra.HTTP.Host != "env-host:5678" {
		t.Errorf("host = %q, want env-host:5678 (env must take priority over file)", cfg.Infra.HTTP.Host)
	}
	if cfg.Worker.Pool.Size != 7 {
		t.Errorf("worker.pool.size = %d, want 7 from environment", cfg.Worker.Pool.Size)
	}
	if cfg.Infra.PostgreSQL.DSN != "file-dsn" {
		t.Errorf("dsn = %q, want file-dsn (file backs keys absent from env)", cfg.Infra.PostgreSQL.DSN)
	}
	if cfg.Infra.HTTP.SwaggerEnabled {
		t.Error("swagger_enabled = true, want false from config file")
	}
}

// Keys the file omits still resolve from the environment when a file is
// present. Under the old file-XOR-env switch these fell back to defaults;
// the merge reads them from env.
func TestLoadEnvSuppliesKeysMissingFromFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
`)
	setenv(t, "BEELINEWF_WORKER_POOL_SIZE", "7")

	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Worker.Pool.Size != 7 {
		t.Errorf("worker.pool.size = %d, want 7 from environment", cfg.Worker.Pool.Size)
	}
}

// The BEELINEWF_API_TOKEN alias keeps working with a file present: the
// explicit binding wins over the file value.
func TestLoadAPITokenAliasOverridesFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
auth:
  enabled: true
  api_token: "file-token"
`)
	setenv(t, "BEELINEWF_API_TOKEN", "env-token")

	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Auth.APIToken != "env-token" {
		t.Errorf("auth.api_token = %q, want env-token from BEELINEWF_API_TOKEN", cfg.Auth.APIToken)
	}
}

// A set-but-empty variable is treated as unset (Viper's default), so the
// file value stands. This pins the behavior operators get when a compose
// file interpolates an unset variable to "".
func TestLoadEmptyEnvFallsBackToFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  http:
    host: "file-host:1234"
  postgresql:
    dsn: "file-dsn"
`)
	setenv(t, "BEELINEWF_INFRA_HTTP_HOST", "")

	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Infra.HTTP.Host != "file-host:1234" {
		t.Errorf("host = %q, want file-host:1234 (empty env must fall back to file)", cfg.Infra.HTTP.Host)
	}
}

func TestLoadDefaultsApplied(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Infra.HTTP.Host != "localhost:8080" {
		t.Errorf("default host = %q, want localhost:8080", cfg.Infra.HTTP.Host)
	}
	if !cfg.Infra.HTTP.SwaggerEnabled {
		t.Error("default swagger_enabled = false, want true")
	}
	if cfg.Worker.Pool.Size != 1000 {
		t.Errorf("default pool size = %d, want 1000", cfg.Worker.Pool.Size)
	}
	if cfg.Infra.HTTP.MaxBodyBytes != 1048576 {
		t.Errorf("default max_body_bytes = %d, want 1048576", cfg.Infra.HTTP.MaxBodyBytes)
	}
}

func TestLoadMaxBodyBytesEnvOverridesFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  http:
    max_body_bytes: 2048
  postgresql:
    dsn: "file-dsn"
`)
	setenv(t, "BEELINEWF_INFRA_HTTP_MAX_BODY_BYTES", "4096")

	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Infra.HTTP.MaxBodyBytes != 4096 {
		t.Errorf("max_body_bytes = %d, want 4096 from environment", cfg.Infra.HTTP.MaxBodyBytes)
	}
}

func TestLoadRejectsNonPositiveMaxBodyBytes(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		t.Run(value, func(t *testing.T) {
			setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
			setenv(t, "BEELINEWF_INFRA_HTTP_MAX_BODY_BYTES", value)
			if _, err := configuration.Load(); err == nil {
				t.Fatalf("Load() error = nil, want error for max_body_bytes=%s", value)
			}
		})
	}
}

func TestLoadServerTimeoutDefaults(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Infra.HTTP.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("read_header_timeout = %v, want 10s", cfg.Infra.HTTP.ReadHeaderTimeout)
	}
	if cfg.Infra.HTTP.IdleTimeout != 60*time.Second {
		t.Errorf("idle_timeout = %v, want 60s", cfg.Infra.HTTP.IdleTimeout)
	}
}

func TestLoadServerTimeoutEnvOverridesFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  http:
    read_header_timeout: 1s
    idle_timeout: 2s
  postgresql:
    dsn: "file-dsn"
`)
	setenv(t, "BEELINEWF_INFRA_HTTP_READ_HEADER_TIMEOUT", "3s")
	setenv(t, "BEELINEWF_INFRA_HTTP_IDLE_TIMEOUT", "4s")

	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Infra.HTTP.ReadHeaderTimeout != 3*time.Second {
		t.Errorf("read_header_timeout = %v, want 3s from environment", cfg.Infra.HTTP.ReadHeaderTimeout)
	}
	if cfg.Infra.HTTP.IdleTimeout != 4*time.Second {
		t.Errorf("idle_timeout = %v, want 4s from environment", cfg.Infra.HTTP.IdleTimeout)
	}
}

func TestLoadRejectsNonPositiveServerTimeouts(t *testing.T) {
	cases := []struct {
		env  string
		name string
	}{
		{"BEELINEWF_INFRA_HTTP_READ_HEADER_TIMEOUT", "infra.http.read_header_timeout"},
		{"BEELINEWF_INFRA_HTTP_IDLE_TIMEOUT", "infra.http.idle_timeout"},
	}
	for _, tc := range cases {
		for _, value := range []string{"0s", "-1s"} {
			t.Run(tc.env+"="+value, func(t *testing.T) {
				setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
				setenv(t, tc.env, value)
				_, err := configuration.Load()
				if err == nil {
					t.Fatalf("Load() error = nil, want error for %s=%s", tc.name, value)
				} else if !strings.Contains(err.Error(), tc.name) {
					t.Errorf("error = %q, want mention of %s", err, tc.name)
				}
			})
		}
	}
}

func TestLoadDispatcherTuningDefaults(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Worker.StatusPool.Size != 20 {
		t.Errorf("worker.status_pool.size = %d, want 20", cfg.Worker.StatusPool.Size)
	}
	if cfg.Engine.PollInterval != 200*time.Millisecond {
		t.Errorf("engine.poll_interval = %v, want 200ms", cfg.Engine.PollInterval)
	}
	if cfg.Engine.HeartbeatInterval != 5*time.Second {
		t.Errorf("engine.heartbeat_interval = %v, want 5s", cfg.Engine.HeartbeatInterval)
	}
}

func TestLoadDispatcherTuningFromEnv(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_WORKER_STATUS_POOL_SIZE", "7")
	setenv(t, "BEELINEWF_ENGINE_POLL_INTERVAL", "50ms")
	setenv(t, "BEELINEWF_ENGINE_HEARTBEAT_INTERVAL", "9s")

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Worker.StatusPool.Size != 7 {
		t.Errorf("worker.status_pool.size = %d, want 7", cfg.Worker.StatusPool.Size)
	}
	if cfg.Engine.PollInterval != 50*time.Millisecond {
		t.Errorf("engine.poll_interval = %v, want 50ms", cfg.Engine.PollInterval)
	}
	if cfg.Engine.HeartbeatInterval != 9*time.Second {
		t.Errorf("engine.heartbeat_interval = %v, want 9s", cfg.Engine.HeartbeatInterval)
	}
}

func TestLoadRejectsNonPositiveDispatcherTuning(t *testing.T) {
	cases := []struct {
		env    string
		name   string
		values []string
	}{
		{"BEELINEWF_WORKER_STATUS_POOL_SIZE", "worker.status_pool.size", []string{"0", "-2"}},
		{"BEELINEWF_ENGINE_POLL_INTERVAL", "engine.poll_interval", []string{"0", "-5s"}},
		{"BEELINEWF_ENGINE_HEARTBEAT_INTERVAL", "engine.heartbeat_interval", []string{"0", "-5s"}},
	}
	for _, tc := range cases {
		for _, value := range tc.values {
			t.Run(tc.env+"="+value, func(t *testing.T) {
				setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
				setenv(t, tc.env, value)
				if _, err := configuration.Load(); err == nil {
					t.Errorf("%s = %q: error = nil, want a rejection", tc.env, value)
				} else if !strings.Contains(err.Error(), tc.name) {
					t.Errorf("%s = %q: error = %q, want mention of %s", tc.env, value, err, tc.name)
				}
			})
		}
	}
}

func TestLoadRejectsMissingDSN(t *testing.T) {
	_, err := configuration.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want validation error for missing dsn")
	}
	if !strings.Contains(err.Error(), "dsn") {
		t.Errorf("error = %q, want mention of dsn", err)
	}
}

func TestLoadRejectsInvalidPoolSize(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_WORKER_POOL_SIZE", "0")

	_, err := configuration.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want validation error for pool size 0")
	}
}

func TestLoadRejectsInvalidDuration(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_ENGINE_POLL_INTERVAL", "not-a-duration")

	_, err := configuration.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want error for invalid duration")
	}
}

func TestLoadRejectsInvalidConfigFile(t *testing.T) {
	path := writeConfig(t, "::not: [valid yaml")

	_, err := configuration.Load(configuration.WithConfigFile(path))
	if err == nil {
		t.Fatal("Load() error = nil, want error for invalid yaml")
	}
}

func TestLoadEngineDefaults(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Engine.DefaultNodeTimeout != 30*time.Second {
		t.Errorf("default_node_timeout = %v, want 30s", cfg.Engine.DefaultNodeTimeout)
	}
	if cfg.Engine.MaxNodeTimeout != 5*time.Minute {
		t.Errorf("max_node_timeout = %v, want 5m", cfg.Engine.MaxNodeTimeout)
	}
	if cfg.Engine.ConditionTimeout != 5*time.Second {
		t.Errorf("condition_timeout = %v, want 5s", cfg.Engine.ConditionTimeout)
	}
	if cfg.System.UserID == "" || cfg.System.Name != "system" || cfg.System.Email == "" {
		t.Errorf("system defaults mismatch: %+v", cfg.System)
	}
}

func TestLoadLeanContextEnvDefaultOverridesConfigFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
engine:
  lean_context_default: false
`)
	setenv(t, "BEELINEWF_ENGINE_LEAN_CONTEXT_DEFAULT", "true")

	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Engine.LeanContextDefault {
		t.Error("lean_context_default = false, want true from environment")
	}
	if cfg.Engine.LeanAnchorEvery != 20 {
		t.Errorf("lean_anchor_every = %d, want 20", cfg.Engine.LeanAnchorEvery)
	}
	if cfg.Engine.LeanReplayMax != 500 {
		t.Errorf("lean_replay_max = %d, want 500", cfg.Engine.LeanReplayMax)
	}
}

func TestLoadEngineSettingsFromEnv(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_ENGINE_MAX_PER_NODE_EXECUTIONS", "50")
	setenv(t, "BEELINEWF_ENGINE_MAX_TOTAL_EXECUTIONS", "500")
	setenv(t, "BEELINEWF_ENGINE_LEASE_DURATION", "45s")
	setenv(t, "BEELINEWF_ENGINE_CLAIM_BATCH_SIZE", "7")
	setenv(t, "BEELINEWF_ENGINE_MAX_OUTPUT_BYTES", "2048")
	setenv(t, "BEELINEWF_ENGINE_MAX_REDIRECTS", "3")
	setenv(t, "BEELINEWF_ENGINE_HTTP_ALLOWLIST", "*")
	setenv(t, "BEELINEWF_ENGINE_EXEC_ALLOWLIST", "echo")

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Engine.MaxPerNodeExecutions != 50 {
		t.Errorf("max_per_node_executions = %d, want 50", cfg.Engine.MaxPerNodeExecutions)
	}
	if cfg.Engine.MaxTotalExecutions != 500 {
		t.Errorf("max_total_executions = %d, want 500", cfg.Engine.MaxTotalExecutions)
	}
	if cfg.Engine.LeaseDuration != 45*time.Second {
		t.Errorf("lease_duration = %v, want 45s", cfg.Engine.LeaseDuration)
	}
	if cfg.Engine.ClaimBatchSize != 7 {
		t.Errorf("claim_batch_size = %d, want 7", cfg.Engine.ClaimBatchSize)
	}
	if cfg.Engine.MaxOutputBytes != 2048 {
		t.Errorf("max_output_bytes = %d, want 2048", cfg.Engine.MaxOutputBytes)
	}
	if cfg.Engine.MaxRedirects != 3 {
		t.Errorf("max_redirects = %d, want 3", cfg.Engine.MaxRedirects)
	}
	if len(cfg.Engine.HTTPAllowlist) != 1 || cfg.Engine.HTTPAllowlist[0] != "*" {
		t.Errorf("http_allowlist = %q, want [\"*\"]", cfg.Engine.HTTPAllowlist)
	}
	if len(cfg.Engine.ExecAllowlist) != 1 || cfg.Engine.ExecAllowlist[0] != "echo" {
		t.Errorf("exec_allowlist = %q, want [\"echo\"]", cfg.Engine.ExecAllowlist)
	}
}

func TestLoadEngineAllowlistsFromEnvCommaSeparated(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_ENGINE_HTTP_ALLOWLIST", "api.example.com,jsonplaceholder.typicode.com")
	setenv(t, "BEELINEWF_ENGINE_EXEC_ALLOWLIST", "echo,ls")

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !slices.Equal(cfg.Engine.HTTPAllowlist, []string{"api.example.com", "jsonplaceholder.typicode.com"}) {
		t.Errorf("http_allowlist = %q, want [api.example.com jsonplaceholder.typicode.com]", cfg.Engine.HTTPAllowlist)
	}
	if !slices.Equal(cfg.Engine.ExecAllowlist, []string{"echo", "ls"}) {
		t.Errorf("exec_allowlist = %q, want [echo ls]", cfg.Engine.ExecAllowlist)
	}
}

func TestLoadEnvDenyOverridesFromEnvCommaSeparated(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_ENGINE_ENV_DENY_EXTRA", "BEELINEWF_OPENROUTER*,BEELINEWF_FOO")
	setenv(t, "BEELINEWF_ENGINE_ENV_ALLOW_EXCEPTIONS", "BEELINEWF_OPENROUTER_MODEL")

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !slices.Equal(cfg.Engine.EnvDenyExtra, []string{"BEELINEWF_OPENROUTER*", "BEELINEWF_FOO"}) {
		t.Errorf("env_deny_extra = %q", cfg.Engine.EnvDenyExtra)
	}
	if !slices.Equal(cfg.Engine.EnvAllowExceptions, []string{"BEELINEWF_OPENROUTER_MODEL"}) {
		t.Errorf("env_allow_exceptions = %q", cfg.Engine.EnvAllowExceptions)
	}
}

func TestLoadRejectsInvalidEnvDenyGlob(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_ENGINE_ENV_DENY_EXTRA", "BEELINEWF_[")
	if _, err := configuration.Load(); err == nil {
		t.Fatal("Load() error = nil, want error for malformed glob")
	}
	setenv(t, "BEELINEWF_ENGINE_ENV_DENY_EXTRA", "")
	setenv(t, "BEELINEWF_ENGINE_ENV_ALLOW_EXCEPTIONS", "BEELINEWF_*[")
	if _, err := configuration.Load(); err == nil {
		t.Fatal("Load() error = nil, want error for malformed allow glob")
	}
}

func TestLoadRejectsMaxBelowDefault(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_ENGINE_DEFAULT_NODE_TIMEOUT", "10m")
	setenv(t, "BEELINEWF_ENGINE_MAX_NODE_TIMEOUT", "5m")
	if _, err := configuration.Load(); err == nil {
		t.Fatal("Load() error = nil, want error when max < default")
	}
}

func TestLoadBrokerDefaults(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Infra.Redis.DSN != "" {
		t.Errorf("redis dsn = %q, want empty (optional)", cfg.Infra.Redis.DSN)
	}
	if cfg.Infra.RabbitMQ.DSN != "" {
		t.Errorf("rabbitmq dsn = %q, want empty (optional)", cfg.Infra.RabbitMQ.DSN)
	}
	if cfg.Infra.RabbitMQ.InputQueue != "beelinewf.input" {
		t.Errorf("input_queue = %q, want beelinewf.input", cfg.Infra.RabbitMQ.InputQueue)
	}
	if cfg.Infra.RabbitMQ.OutputQueue != "beelinewf.output" {
		t.Errorf("output_queue = %q, want beelinewf.output", cfg.Infra.RabbitMQ.OutputQueue)
	}
	if cfg.Infra.RabbitMQ.StatusQueue != "beelinewf.status" {
		t.Errorf("status_queue = %q, want beelinewf.status", cfg.Infra.RabbitMQ.StatusQueue)
	}
}

func TestLoadBrokersFromEnv(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_INFRA_REDIS_DSN", "redis://localhost:6379/0")
	setenv(t, "BEELINEWF_INFRA_RABBITMQ_DSN", "amqp://beelinewf:beelinewf@localhost:5672/")
	setenv(t, "BEELINEWF_INFRA_RABBITMQ_INPUT_QUEUE", "workflow.input")
	setenv(t, "BEELINEWF_INFRA_RABBITMQ_OUTPUT_QUEUE", "workflow.output")
	setenv(t, "BEELINEWF_INFRA_RABBITMQ_STATUS_QUEUE", "workflow.status")

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Infra.Redis.DSN != "redis://localhost:6379/0" {
		t.Errorf("redis dsn = %q, want redis://localhost:6379/0", cfg.Infra.Redis.DSN)
	}
	if cfg.Infra.RabbitMQ.DSN != "amqp://beelinewf:beelinewf@localhost:5672/" {
		t.Errorf("rabbitmq dsn = %q, want amqp://...", cfg.Infra.RabbitMQ.DSN)
	}
	if cfg.Infra.RabbitMQ.InputQueue != "workflow.input" || cfg.Infra.RabbitMQ.OutputQueue != "workflow.output" || cfg.Infra.RabbitMQ.StatusQueue != "workflow.status" {
		t.Errorf("rabbit queues = %+v, want explicit values", cfg.Infra.RabbitMQ)
	}
}

func TestLoadBrokersFromConfigFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
  redis:
    dsn: "redis://file-redis:6379/0"
  rabbitmq:
    dsn: "amqp://file-rabbit:5672/"
`)
	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Infra.Redis.DSN != "redis://file-redis:6379/0" {
		t.Errorf("redis dsn = %q, want file value", cfg.Infra.Redis.DSN)
	}
	if cfg.Infra.RabbitMQ.DSN != "amqp://file-rabbit:5672/" {
		t.Errorf("rabbitmq dsn = %q, want file value", cfg.Infra.RabbitMQ.DSN)
	}
	// Queue defaults still apply when the file omits them.
	if cfg.Infra.RabbitMQ.InputQueue != "beelinewf.input" {
		t.Errorf("input_queue = %q, want default beelinewf.input", cfg.Infra.RabbitMQ.InputQueue)
	}
}

func TestLoadRejectsRabbitDSNWithoutQueues(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
  rabbitmq:
    dsn: "amqp://localhost:5672/"
    input_queue: ""
`)
	_, err := configuration.Load(configuration.WithConfigFile(path))
	if err == nil {
		t.Fatal("Load() error = nil, want error for rabbit dsn with empty input_queue")
	}
	if !strings.Contains(err.Error(), "input_queue") {
		t.Errorf("error = %q, want mention of input_queue", err)
	}
}

// The system user is the audit actor the service bypasses are recorded as,
// so a value that is not a canonical uuid has to fail the boot rather than
// the first foreign key.
//
// An empty value is not in this table: Viper substitutes the default before
// validate runs, so a blank user id in the environment is the shipped
// default rather than a misconfiguration. It is TestShippedConfigUsesAUUID
// that pins the fallback.
func TestLoadRejectsNonUUIDSystemUserID(t *testing.T) {
	for _, id := range []string{"system", "not-a-uuid", "  ", "00000000-0000-7000-8000-00000000000", "00000000000070008000000000000001"} {
		setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
		setenv(t, "BEELINEWF_SYSTEM_USER_ID", id)
		_, err := configuration.Load()
		if err == nil {
			t.Errorf("system.user_id = %q: error = nil, want a rejection", id)
			continue
		}
		if !strings.Contains(err.Error(), "system.user_id") {
			t.Errorf("system.user_id = %q: error = %q, want mention of system.user_id", id, err)
		}
	}
}

// A valid system user id still loads.
func TestLoadAcceptsUUIDSystemUserID(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_SYSTEM_USER_ID", "00000000-0000-7000-8000-000000000001")
	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.System.UserID != "00000000-0000-7000-8000-000000000001" {
		t.Errorf("system.user_id = %q, want the configured uuid", cfg.System.UserID)
	}
}

// The validation is new, so the default the code falls back to has to
// satisfy it: a deployment that never sets system.user_id must still boot.
func TestShippedConfigUsesAUUIDSystemUserByDefault(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_SYSTEM_USER_ID", "")
	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !ids.Valid(cfg.System.UserID) {
		t.Errorf("default system.user_id = %q, want a canonical uuid", cfg.System.UserID)
	}
}

// The shipped config.yaml and the e2e config both name a system user, so
// the new validation must not reject either. config.e2e-oidc.yaml is started
// by `task e2e-oidc`, which exports the API token the same way.
func TestShippedConfigsPassSystemUserValidation(t *testing.T) {
	setenv(t, "BEELINEWF_API_TOKEN", "e2e-service-token")
	for _, name := range []string{"config.yaml", "config.e2e-oidc.yaml"} {
		path := filepath.Join("..", "..", name)
		if _, err := os.Stat(path); err != nil {
			t.Skipf("%s not available: %v", name, err)
		}
		if _, err := configuration.Load(configuration.WithConfigFile(path)); err != nil {
			t.Errorf("Load(%s) error = %v", name, err)
		}
	}
}

// The shipped config.yaml documents clock_skew above the cache_ttl it belongs
// to. A comment describing a key that is not there reads as if the tolerance
// is set, so the file is loaded here to keep the key and its comment together.
func TestShippedConfigParsesClockSkew(t *testing.T) {
	path := filepath.Join("..", "..", "config.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("shipped config not available: %v", err)
	}
	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load(%s) error = %v", path, err)
	}
	if cfg.Auth.OIDC.ClockSkew != time.Minute {
		t.Errorf("auth.oidc.clock_skew = %v, want 1m", cfg.Auth.OIDC.ClockSkew)
	}
}

// Every role the shipped config.yaml grants must have a pre-minted token in
// scripts/mock_oidc_provider.py, otherwise OIDC-mode testing cannot exercise
// that role. The mock may offer more (manager serves the e2e catalog in
// config.e2e-oidc.yaml, noroles the role-less case), but never fewer.
func TestMockOIDCCoversShippedRoles(t *testing.T) {
	// Blank out the JSON override so the file catalog is what is checked,
	// not whatever the ambient environment carries.
	setenv(t, "BEELINEWF_AUTH_ROLE_PERMISSIONS", "")
	path := filepath.Join("..", "..", "config.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("shipped config not available: %v", err)
	}
	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load(%s) error = %v", path, err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "mock_oidc_provider.py"))
	if err != nil {
		t.Fatalf("read mock_oidc_provider.py: %v", err)
	}
	// A pre-minted token reads `"name": token("subject", ["roles"])`.
	for role := range cfg.Auth.RolePermissions {
		if !strings.Contains(string(raw), `"`+role+`": token(`) {
			t.Errorf("shipped role %q has no pre-minted mock token (want /token/%s in scripts/mock_oidc_provider.py)", role, role)
		}
	}
}

// The role catalog is a map, which cannot ride Viper's AutomaticEnv, so it
// is exposed through an explicit post-unmarshal JSON override.
func TestLoadRolePermissionsFromEnvJSON(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_AUTH_ROLE_PERMISSIONS", `{"admin":["definitions:read","roles:read"],"finance":["instances:read"]}`)

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Auth.RolePermissions["admin"]; !slices.Equal(got, []string{"definitions:read", "roles:read"}) {
		t.Errorf("admin = %v, want [definitions:read roles:read]", got)
	}
	if got := cfg.Auth.RolePermissions["finance"]; !slices.Equal(got, []string{"instances:read"}) {
		t.Errorf("finance = %v, want [instances:read]", got)
	}
}

func TestLoadRolePermissionsEnvOverridesFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
auth:
  role_permissions:
    admin: ["definitions:read"]
`)
	setenv(t, "BEELINEWF_AUTH_ROLE_PERMISSIONS", `{"finance":["instances:read"]}`)

	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Auth.RolePermissions) != 1 || cfg.Auth.RolePermissions["finance"] == nil {
		t.Errorf("RolePermissions = %v, want only env value {finance:...}", cfg.Auth.RolePermissions)
	}
}

func TestLoadRolePermissionsInvalidJSONFails(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_AUTH_ROLE_PERMISSIONS", `not-json`)

	_, err := configuration.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want fail-fast on invalid JSON")
	}
	if !strings.Contains(err.Error(), "BEELINEWF_AUTH_ROLE_PERMISSIONS") {
		t.Errorf("error = %q, want it to name BEELINEWF_AUTH_ROLE_PERMISSIONS", err.Error())
	}
}

func TestLoadRolePermissionsBlankEnvUsesFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
auth:
  role_permissions:
    admin: ["definitions:read"]
`)
	setenv(t, "BEELINEWF_AUTH_ROLE_PERMISSIONS", "   ")

	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !slices.Equal(cfg.Auth.RolePermissions["admin"], []string{"definitions:read"}) {
		t.Errorf("admin = %v, want file value [definitions:read]", cfg.Auth.RolePermissions["admin"])
	}
}

func TestLoadSchedulerDefaults(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Scheduler.Enabled {
		t.Error("scheduler.enabled = false, want true by default")
	}
	if cfg.Scheduler.RefreshInterval != 30*time.Second {
		t.Errorf("scheduler.refresh_interval = %v, want 30s", cfg.Scheduler.RefreshInterval)
	}
}

func TestLoadSchedulerFromEnv(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_SCHEDULER_ENABLED", "false")
	setenv(t, "BEELINEWF_SCHEDULER_REFRESH_INTERVAL", "10s")

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Scheduler.Enabled {
		t.Error("scheduler.enabled = true, want false from environment")
	}
	if cfg.Scheduler.RefreshInterval != 10*time.Second {
		t.Errorf("scheduler.refresh_interval = %v, want 10s", cfg.Scheduler.RefreshInterval)
	}
}

func TestLoadRejectsInvalidSchedulerRefreshInterval(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_SCHEDULER_REFRESH_INTERVAL", "0s")

	if _, err := configuration.Load(); err == nil {
		t.Fatal("Load() error = nil, want validation error for refresh_interval 0")
	}
}

func TestLoadConsumerRetryDefaults(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	r := cfg.Engine.ConsumerRetry
	if r.MaxAttempts != 3 || r.InitialBackoff != time.Second || r.MaxBackoff != 30*time.Second || r.MinHealthyRun != 30*time.Second {
		t.Errorf("consumer_retry = %+v, want 3/1s/30s/30s", r)
	}
}

func TestLoadConsumerRetryFromFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
engine:
  consumer_retry:
    max_attempts: 5
    initial_backoff: 2s
    max_backoff: 1m
    min_healthy_run: 45s
`)
	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	r := cfg.Engine.ConsumerRetry
	if r.MaxAttempts != 5 || r.InitialBackoff != 2*time.Second || r.MaxBackoff != time.Minute || r.MinHealthyRun != 45*time.Second {
		t.Errorf("consumer_retry = %+v, want 5/2s/1m/45s", r)
	}
}

func TestLoadConsumerRetryEnvOverridesFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
engine:
  consumer_retry:
    max_attempts: 5
    initial_backoff: 2s
    max_backoff: 1m
    min_healthy_run: 45s
`)
	setenv(t, "BEELINEWF_ENGINE_CONSUMER_RETRY_MAX_ATTEMPTS", "7")
	setenv(t, "BEELINEWF_ENGINE_CONSUMER_RETRY_INITIAL_BACKOFF", "500ms")
	setenv(t, "BEELINEWF_ENGINE_CONSUMER_RETRY_MAX_BACKOFF", "10s")
	setenv(t, "BEELINEWF_ENGINE_CONSUMER_RETRY_MIN_HEALTHY_RUN", "20s")

	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	r := cfg.Engine.ConsumerRetry
	if r.MaxAttempts != 7 || r.InitialBackoff != 500*time.Millisecond || r.MaxBackoff != 10*time.Second || r.MinHealthyRun != 20*time.Second {
		t.Errorf("consumer_retry = %+v, want 7/500ms/10s/20s from environment", r)
	}
}

func TestLoadHTTPBulkheadDefaults(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Engine.HTTPMaxInFlight != 128 || cfg.Engine.HTTPMaxInFlightPerHost != 16 {
		t.Errorf("http bulkhead = %d/%d, want 128/16", cfg.Engine.HTTPMaxInFlight, cfg.Engine.HTTPMaxInFlightPerHost)
	}
}

func TestLoadHTTPBulkheadFromFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
engine:
  http_max_in_flight: 64
  http_max_in_flight_per_host: 8
`)
	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Engine.HTTPMaxInFlight != 64 || cfg.Engine.HTTPMaxInFlightPerHost != 8 {
		t.Errorf("http bulkhead = %d/%d, want 64/8 from file", cfg.Engine.HTTPMaxInFlight, cfg.Engine.HTTPMaxInFlightPerHost)
	}
}

func TestLoadHTTPBulkheadEnvOverridesFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
engine:
  http_max_in_flight: 64
  http_max_in_flight_per_host: 8
`)
	setenv(t, "BEELINEWF_ENGINE_HTTP_MAX_IN_FLIGHT", "32")
	setenv(t, "BEELINEWF_ENGINE_HTTP_MAX_IN_FLIGHT_PER_HOST", "4")

	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Engine.HTTPMaxInFlight != 32 || cfg.Engine.HTTPMaxInFlightPerHost != 4 {
		t.Errorf("http bulkhead = %d/%d, want 32/4 from environment", cfg.Engine.HTTPMaxInFlight, cfg.Engine.HTTPMaxInFlightPerHost)
	}
}

func TestLoadRejectsInvalidHTTPBulkhead(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"http_max_in_flight", "http_max_in_flight: 0"},
		{"http_max_in_flight", "http_max_in_flight: -2"},
		{"http_max_in_flight_per_host", "http_max_in_flight_per_host: 0"},
		{"http_max_in_flight_per_host", "http_max_in_flight_per_host: -3"},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
engine:
  `+tc.value+`
`)
			_, err := configuration.Load(configuration.WithConfigFile(path))
			if err == nil {
				t.Fatalf("Load() error = nil, want rejection for %s", tc.value)
			}
			if !strings.Contains(err.Error(), "engine."+tc.name) {
				t.Errorf("error = %q, want it to name engine.%s", err, tc.name)
			}
		})
	}
}

func TestShippedConfigHTTPBulkheadCaps(t *testing.T) {
	path := filepath.Join("..", "..", "config.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("shipped config not available: %v", err)
	}
	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load(%s) error = %v", path, err)
	}
	if cfg.Engine.HTTPMaxInFlight != 128 || cfg.Engine.HTTPMaxInFlightPerHost != 16 {
		t.Errorf("shipped http bulkhead = %d/%d, want 128/16", cfg.Engine.HTTPMaxInFlight, cfg.Engine.HTTPMaxInFlightPerHost)
	}
}

func TestLoadPostgreSQLPoolDefaults(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	pg := cfg.Infra.PostgreSQL
	if pg.MaxOpenConns != 25 || pg.MaxIdleConns != 25 ||
		pg.ConnMaxLifetime != 5*time.Minute || pg.ConnMaxIdleTime != 5*time.Minute {
		t.Errorf("postgresql pool = %+v, want 25/25/5m/5m", pg)
	}
	if pg.ConnectTimeout != 15*time.Second {
		t.Errorf("postgresql connect_timeout = %v, want 15s", pg.ConnectTimeout)
	}
}

func TestLoadOIDCDiscoveryTimeoutDefault(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Auth.OIDC.DiscoveryTimeout != 10*time.Second {
		t.Errorf("auth.oidc.discovery_timeout = %v, want 10s", cfg.Auth.OIDC.DiscoveryTimeout)
	}
}

func TestLoadOIDCDiscoveryTimeoutFromEnv(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_AUTH_ENABLED", "true")
	setenv(t, "BEELINEWF_API_TOKEN", "test-token")
	setenv(t, "BEELINEWF_AUTH_OIDC_ENABLED", "true")
	setenv(t, "BEELINEWF_AUTH_OIDC_ISSUER", "https://issuer.example.com")
	setenv(t, "BEELINEWF_AUTH_OIDC_CLIENT_ID", "beelinewf")
	setenv(t, "BEELINEWF_AUTH_OIDC_DISCOVERY_TIMEOUT", "3s")

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Auth.OIDC.DiscoveryTimeout != 3*time.Second {
		t.Errorf("auth.oidc.discovery_timeout = %v, want 3s from environment", cfg.Auth.OIDC.DiscoveryTimeout)
	}
}

func TestLoadRejectsNonPositiveOIDCDiscoveryTimeout(t *testing.T) {
	for _, value := range []string{"0s", "-1s"} {
		t.Run(value, func(t *testing.T) {
			setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
			setenv(t, "BEELINEWF_AUTH_ENABLED", "true")
			setenv(t, "BEELINEWF_API_TOKEN", "test-token")
			setenv(t, "BEELINEWF_AUTH_OIDC_ENABLED", "true")
			setenv(t, "BEELINEWF_AUTH_OIDC_ISSUER", "https://issuer.example.com")
			setenv(t, "BEELINEWF_AUTH_OIDC_CLIENT_ID", "beelinewf")
			setenv(t, "BEELINEWF_AUTH_OIDC_DISCOVERY_TIMEOUT", value)

			_, err := configuration.Load()
			if err == nil {
				t.Fatalf("Load() error = nil, want error for discovery_timeout=%s", value)
			} else if !strings.Contains(err.Error(), "auth.oidc.discovery_timeout") {
				t.Errorf("error = %q, want mention of auth.oidc.discovery_timeout", err)
			}
		})
	}
}

func TestLoadPostgreSQLPoolFromFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
    max_open_conns: 10
    max_idle_conns: 4
    conn_max_lifetime: 10m
    conn_max_idle_time: 2m
    connect_timeout: 7s
`)
	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	pg := cfg.Infra.PostgreSQL
	if pg.MaxOpenConns != 10 || pg.MaxIdleConns != 4 ||
		pg.ConnMaxLifetime != 10*time.Minute || pg.ConnMaxIdleTime != 2*time.Minute {
		t.Errorf("postgresql pool = %+v, want 10/4/10m/2m from file", pg)
	}
	if pg.ConnectTimeout != 7*time.Second {
		t.Errorf("postgresql connect_timeout = %v, want 7s from file", pg.ConnectTimeout)
	}
}

func TestLoadPostgreSQLPoolEnvOverridesFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
    max_open_conns: 10
    max_idle_conns: 4
    conn_max_lifetime: 10m
    conn_max_idle_time: 2m
`)
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_MAX_OPEN_CONNS", "50")
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_MAX_IDLE_CONNS", "10")
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_CONN_MAX_LIFETIME", "1h")
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_CONN_MAX_IDLE_TIME", "30s")
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_CONNECT_TIMEOUT", "20s")

	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	pg := cfg.Infra.PostgreSQL
	if pg.MaxOpenConns != 50 || pg.MaxIdleConns != 10 ||
		pg.ConnMaxLifetime != time.Hour || pg.ConnMaxIdleTime != 30*time.Second {
		t.Errorf("postgresql pool = %+v, want 50/10/1h/30s from environment", pg)
	}
	if pg.ConnectTimeout != 20*time.Second {
		t.Errorf("postgresql connect_timeout = %v, want 20s from environment", pg.ConnectTimeout)
	}
}

func TestLoadRejectsInvalidPostgreSQLPool(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{"max_open_conns", "max_open_conns", "max_open_conns: 0"},
		{"max_idle_conns", "max_idle_conns", "max_open_conns: 10\n    max_idle_conns: 99"},
		{"conn_max_lifetime", "conn_max_lifetime", "conn_max_lifetime: -5s"},
		{"conn_max_idle_time", "conn_max_idle_time", "conn_max_idle_time: -5s"},
		{"connect_timeout", "connect_timeout", "connect_timeout: 0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
    `+tc.value+`
`)
			_, err := configuration.Load(configuration.WithConfigFile(path))
			if err == nil {
				t.Fatalf("Load() error = nil, want rejection for %s", tc.name)
			}
			if !strings.Contains(err.Error(), "infra.postgresql."+tc.key) {
				t.Errorf("error = %q, want it to name infra.postgresql.%s", err, tc.key)
			}
		})
	}
}

func TestLoadRejectsInvalidConsumerRetry(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"max_attempts", "max_attempts: 0"},
		{"initial_backoff", "initial_backoff: 0s"},
		{"max_backoff", "initial_backoff: 5s\n    max_backoff: 1s"},
		{"min_healthy_run", "min_healthy_run: 0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
engine:
  consumer_retry:
    `+tc.value+`
`)
			_, err := configuration.Load(configuration.WithConfigFile(path))
			if err == nil {
				t.Fatalf("Load() error = nil, want rejection for %s", tc.name)
			}
			if !strings.Contains(err.Error(), "configuration: ") || !strings.Contains(err.Error(), "engine.consumer_retry."+tc.name) {
				t.Errorf("error = %q, want a configuration: engine.consumer_retry.%s error", err, tc.name)
			}
		})
	}
}

// auth.enabled is the master switch: with it off, a stray
// BEELINEWF_AUTH_OIDC_ENABLED=true is ignored and forced off, so /v1 stays
// open and the OIDC issuer/client validation does not trip the boot.
func TestLoadAuthDisabledForcesOIDCOff(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_AUTH_ENABLED", "false")
	setenv(t, "BEELINEWF_AUTH_OIDC_ENABLED", "true")

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want OIDC ignored when auth is disabled", err)
	}
	if cfg.Auth.OIDC.Enabled {
		t.Error("auth.oidc.enabled = true, want false when auth.enabled is false")
	}
}

// CORS allows any origin by default, which suits development: a local
// frontend works with no configuration. Methods, headers, and max_age
// carry matching ready-to-use defaults.
func TestLoadCORSAllowsAllOriginsByDefault(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if want := []string{"*"}; !slices.Equal(cfg.Infra.HTTP.CORS.AllowedOrigins, want) {
		t.Errorf("allowed_origins = %v, want %v", cfg.Infra.HTTP.CORS.AllowedOrigins, want)
	}
	if want := []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"}; !slices.Equal(cfg.Infra.HTTP.CORS.AllowedMethods, want) {
		t.Errorf("allowed_methods = %v, want %v", cfg.Infra.HTTP.CORS.AllowedMethods, want)
	}
	if want := []string{"Authorization", "Content-Type", "X-Api-Token"}; !slices.Equal(cfg.Infra.HTTP.CORS.AllowedHeaders, want) {
		t.Errorf("allowed_headers = %v, want %v", cfg.Infra.HTTP.CORS.AllowedHeaders, want)
	}
	if len(cfg.Infra.HTTP.CORS.ExposedHeaders) != 0 {
		t.Errorf("exposed_headers = %v, want empty", cfg.Infra.HTTP.CORS.ExposedHeaders)
	}
	if cfg.Infra.HTTP.CORS.AllowCredentials {
		t.Error("allow_credentials = true, want false")
	}
	if cfg.Infra.HTTP.CORS.MaxAge != 12*time.Hour {
		t.Errorf("max_age = %v, want 12h", cfg.Infra.HTTP.CORS.MaxAge)
	}
}

// Every CORS key is env-addressable, and list values ride the same
// comma-separated encoding as the engine allowlists.
func TestLoadCORSFromEnv(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_INFRA_HTTP_CORS_ALLOWED_ORIGINS", "https://app.example.com,http://localhost:5173")
	setenv(t, "BEELINEWF_INFRA_HTTP_CORS_ALLOWED_METHODS", "GET,POST")
	setenv(t, "BEELINEWF_INFRA_HTTP_CORS_ALLOWED_HEADERS", "Authorization")
	setenv(t, "BEELINEWF_INFRA_HTTP_CORS_EXPOSED_HEADERS", "X-Total")
	setenv(t, "BEELINEWF_INFRA_HTTP_CORS_ALLOW_CREDENTIALS", "true")
	setenv(t, "BEELINEWF_INFRA_HTTP_CORS_MAX_AGE", "1h")

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	cors := cfg.Infra.HTTP.CORS
	if want := []string{"https://app.example.com", "http://localhost:5173"}; !slices.Equal(cors.AllowedOrigins, want) {
		t.Errorf("allowed_origins = %v, want %v", cors.AllowedOrigins, want)
	}
	if want := []string{"GET", "POST"}; !slices.Equal(cors.AllowedMethods, want) {
		t.Errorf("allowed_methods = %v, want %v", cors.AllowedMethods, want)
	}
	if want := []string{"Authorization"}; !slices.Equal(cors.AllowedHeaders, want) {
		t.Errorf("allowed_headers = %v, want %v", cors.AllowedHeaders, want)
	}
	if want := []string{"X-Total"}; !slices.Equal(cors.ExposedHeaders, want) {
		t.Errorf("exposed_headers = %v, want %v", cors.ExposedHeaders, want)
	}
	if !cors.AllowCredentials {
		t.Error("allow_credentials = false, want true")
	}
	if cors.MaxAge != time.Hour {
		t.Errorf("max_age = %v, want 1h", cors.MaxAge)
	}
}

// An explicit empty list disables CORS: the engine emits no
// Access-Control-* headers. Only the file can express this; a set-but-empty
// environment variable falls back to the default like any other key.
func TestLoadCORSEmptyListDisables(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
  http:
    cors:
      allowed_origins: []
`)
	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Infra.HTTP.CORS.AllowedOrigins) != 0 {
		t.Errorf("allowed_origins = %v, want empty (disabled)", cfg.Infra.HTTP.CORS.AllowedOrigins)
	}
}

func TestLoadCORSFromFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
  http:
    cors:
      allowed_origins:
        - "https://app.example.com"
      allow_credentials: true
      max_age: 30m
`)
	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	cors := cfg.Infra.HTTP.CORS
	if want := []string{"https://app.example.com"}; !slices.Equal(cors.AllowedOrigins, want) {
		t.Errorf("allowed_origins = %v, want %v", cors.AllowedOrigins, want)
	}
	if !cors.AllowCredentials {
		t.Error("allow_credentials = false, want true from config file")
	}
	if cors.MaxAge != 30*time.Minute {
		t.Errorf("max_age = %v, want 30m", cors.MaxAge)
	}
}

func TestLoadCORSEnvOverridesFile(t *testing.T) {
	path := writeConfig(t, `
infra:
  postgresql:
    dsn: "file-dsn"
  http:
    cors:
      allowed_origins:
        - "https://file.example.com"
`)
	setenv(t, "BEELINEWF_INFRA_HTTP_CORS_ALLOWED_ORIGINS", "https://env.example.com")

	cfg, err := configuration.Load(configuration.WithConfigFile(path))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if want := []string{"https://env.example.com"}; !slices.Equal(cfg.Infra.HTTP.CORS.AllowedOrigins, want) {
		t.Errorf("allowed_origins = %v, want %v (env must win over file)", cfg.Infra.HTTP.CORS.AllowedOrigins, want)
	}
}

// A credentialed response must name the origin: "*" with credentials on
// would promise browsers something the protocol forbids.
func TestLoadRejectsCORSWildcardWithCredentials(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_INFRA_HTTP_CORS_ALLOWED_ORIGINS", "*")
	setenv(t, "BEELINEWF_INFRA_HTTP_CORS_ALLOW_CREDENTIALS", "true")

	_, err := configuration.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want rejection of \"*\" with credentials")
	}
	if !strings.Contains(err.Error(), "infra.http.cors") {
		t.Errorf("error = %q, want it to name the infra.http.cors key", err)
	}
}

func TestLoadRejectsMalformedCORSOrigin(t *testing.T) {
	cases := []struct {
		name   string
		origin string
	}{
		{"missing scheme", "app.example.com"},
		{"non-http scheme", "ftp://app.example.com"},
		{"path", "https://app.example.com/api"},
		{"query", "https://app.example.com?x=1"},
		{"empty host", "https:///api"},
		{"blank entry", "   "},
		{"wildcard mixed with explicit", "*,https://app.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
			setenv(t, "BEELINEWF_INFRA_HTTP_CORS_ALLOWED_ORIGINS", tc.origin)

			_, err := configuration.Load()
			if err == nil {
				t.Fatalf("Load() error = nil, want rejection of origin %q", tc.origin)
			}
			if !strings.Contains(err.Error(), "infra.http.cors.allowed_origins") {
				t.Errorf("error = %q, want it to name infra.http.cors.allowed_origins", err)
			}
		})
	}
}

func TestLoadRejectsNegativeCORSMaxAge(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_INFRA_HTTP_CORS_ALLOWED_ORIGINS", "https://app.example.com")
	setenv(t, "BEELINEWF_INFRA_HTTP_CORS_MAX_AGE", "-1s")

	_, err := configuration.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want rejection of negative max_age")
	}
	if !strings.Contains(err.Error(), "infra.http.cors.max_age") {
		t.Errorf("error = %q, want it to name infra.http.cors.max_age", err)
	}
}

// Object storage is disabled unless endpoint and both keys are set: a
// deployment that never configures it boots with uploads off, and region /
// use_ssl still carry safe defaults for the day it is enabled.
func TestLoadStorageDefaultsToDisabled(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Infra.Storage.Enabled() {
		t.Errorf("storage = %+v, want disabled by default", cfg.Infra.Storage)
	}
	if cfg.Infra.Storage.Region != "us-east-1" {
		t.Errorf("region = %q, want us-east-1", cfg.Infra.Storage.Region)
	}
	if !cfg.Infra.Storage.UseSSL {
		t.Error("use_ssl = false, want true by default")
	}
}

// A partial storage connection fails the boot instead of failing the first
// multipart delivery: endpoint without credentials, or credentials without
// an endpoint, is a misconfiguration, not a disabled storage.
func TestLoadRejectsPartialStorage(t *testing.T) {
	cases := map[string]map[string]string{
		"endpoint without keys": {
			"BEELINEWF_INFRA_STORAGE_ENDPOINT": "rustfs:9000",
		},
		"endpoint without secret": {
			"BEELINEWF_INFRA_STORAGE_ENDPOINT":   "rustfs:9000",
			"BEELINEWF_INFRA_STORAGE_ACCESS_KEY": "ak",
		},
		"keys without endpoint": {
			"BEELINEWF_INFRA_STORAGE_ACCESS_KEY": "ak",
			"BEELINEWF_INFRA_STORAGE_SECRET_KEY": "sk",
		},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
			for k, v := range env {
				setenv(t, k, v)
			}
			_, err := configuration.Load()
			if err == nil {
				t.Fatal("Load() error = nil, want the partial storage rejected")
			}
			if !strings.Contains(err.Error(), "infra.storage") {
				t.Errorf("error = %q, want it to name infra.storage", err)
			}
		})
	}
}

// Every storage key rides the BEELINEWF_INFRA_STORAGE_* environment
// contract, so compose wires RustFS without rewriting config files.
func TestLoadStorageFromEnv(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_INFRA_STORAGE_ENDPOINT", "rustfs:9000")
	setenv(t, "BEELINEWF_INFRA_STORAGE_REGION", "eu-west-1")
	setenv(t, "BEELINEWF_INFRA_STORAGE_USE_SSL", "false")
	setenv(t, "BEELINEWF_INFRA_STORAGE_ACCESS_KEY", "ak")
	setenv(t, "BEELINEWF_INFRA_STORAGE_SECRET_KEY", "sk")

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	st := cfg.Infra.Storage
	if !st.Enabled() {
		t.Error("storage is disabled, want the env connection enabled")
	}
	if st.Endpoint != "rustfs:9000" || st.Region != "eu-west-1" || st.UseSSL ||
		st.AccessKey != "ak" || st.SecretKey != "sk" {
		t.Errorf("storage = %+v, want the env values", st)
	}
}

// The multipart streaming limits default to bounded values and reject a
// non-positive override: a zero cap would refuse every file, a negative one
// is meaningless.
func TestLoadMultipartLimitsDefaults(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)

	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	mp := cfg.Engine.Multipart
	if mp.MaxFiles != 20 || mp.MaxTotalBytes != 134217728 ||
		mp.MaxFileBytes != 33554432 || mp.MaxTextBytes != 1048576 {
		t.Errorf("multipart = %+v, want 20 files / 128MiB total / 32MiB per file / 1MiB per text part", mp)
	}
}

func TestLoadRejectsNonPositiveMultipartLimits(t *testing.T) {
	for _, key := range []string{
		"BEELINEWF_ENGINE_MULTIPART_MAX_FILES",
		"BEELINEWF_ENGINE_MULTIPART_MAX_TOTAL_BYTES",
		"BEELINEWF_ENGINE_MULTIPART_MAX_FILE_BYTES",
		"BEELINEWF_ENGINE_MULTIPART_MAX_TEXT_BYTES",
	} {
		t.Run(key, func(t *testing.T) {
			setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
			setenv(t, key, "0")
			_, err := configuration.Load()
			if err == nil {
				t.Fatalf("Load() error = nil, want rejection of %s=0", key)
			}
			if !strings.Contains(err.Error(), "engine.multipart") {
				t.Errorf("error = %q, want it to name engine.multipart", err)
			}
		})
	}
}

// The shipped configs document the storage section disabled plus the
// multipart limits, so a fresh checkout boots and the keys stay greppable.
func TestShippedConfigsCarryStorageAndMultipart(t *testing.T) {
	setenv(t, "BEELINEWF_API_TOKEN", "e2e-service-token")
	for _, name := range []string{"config.yaml", "config.e2e.yaml"} {
		path := filepath.Join("..", "..", name)
		if _, err := os.Stat(path); err != nil {
			t.Skipf("%s not available: %v", name, err)
		}
		cfg, err := configuration.Load(configuration.WithConfigFile(path))
		if err != nil {
			t.Errorf("Load(%s) error = %v", name, err)
			continue
		}
		if cfg.Infra.Storage.Enabled() {
			t.Errorf("%s: storage is enabled, want the shipped config disabled", name)
		}
		mp := cfg.Engine.Multipart
		if mp.MaxFiles != 20 || mp.MaxTotalBytes != 134217728 ||
			mp.MaxFileBytes != 33554432 || mp.MaxTextBytes != 1048576 {
			t.Errorf("%s: multipart = %+v, want the documented defaults", name, mp)
		}
	}
}

func TestMetricsEnabledDefaultsTrue(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Infra.HTTP.MetricsEnabled {
		t.Error("metrics_enabled = false, want true by default")
	}
}

func TestMetricsEnabledEnvOverride(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	setenv(t, "BEELINEWF_INFRA_HTTP_METRICS_ENABLED", "false")
	cfg, err := configuration.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Infra.HTTP.MetricsEnabled {
		t.Error("metrics_enabled = true, want false from environment")
	}
}

func TestLoadExplicitMissingPathFails(t *testing.T) {
	setenv(t, "BEELINEWF_INFRA_POSTGRESQL_DSN", testDSN)
	if _, err := configuration.Load(configuration.WithConfigFile(missingPath(t))); err == nil {
		t.Error("Load(explicit missing path) error = nil, want fail-fast error")
	}
}

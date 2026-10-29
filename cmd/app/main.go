// Command app is the composition root of the workflow engine: it loads
// configuration, opens the database, seeds the system user, wires the HTTP
// router and the leased dispatcher, and coordinates graceful shutdown. It
// contains no workflow rules.
package main

import (
	// @title BeelineWF API
	// @version 1.0
	// @description Workflow definition and execution API.
	// @BasePath /
	// @schemes http
	// @securityDefinitions.apikey ApiKeyAuth
	// @in header
	// @name X-Api-Token
	// @description API token authentication via the X-Api-Token header. The token authenticates as the service principal, which bypasses every authorization gate. Failed presentations are throttled per client IP (auth.rate_limit): past the configured threshold further attempts answer 429 with a Retry-After until the block expires.
	// @securityDefinitions.apikey BearerAuth
	// @in header
	// @name Authorization
	// @description Bearer token: an OIDC JWT or a locally issued one. In the Swagger Authorize dialog type "Bearer <jwt>" with the Bearer prefix and a space: the value is sent verbatim as the Authorization header. For OIDC the engine is a resource server: the frontend completes a code+PKCE flow against the provider and calls the API with the JWT. For local auth the frontend logs in over POST /v1/auth/login. Which logins exist is served by GET /v1/auth/config.
	// @type apiKey

	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/engine"
	"github.com/beelinewf/wf-engine/internal/workflow/executor"
	"github.com/beelinewf/wf-engine/internal/workflow/handler"
	"github.com/beelinewf/wf-engine/internal/workflow/inputtransport"
	"github.com/beelinewf/wf-engine/internal/workflow/mail"
	"github.com/beelinewf/wf-engine/internal/workflow/metrics"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/multipart"
	"github.com/beelinewf/wf-engine/internal/workflow/objectstore"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/internal/workflow/scheduler"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"github.com/beelinewf/wf-engine/internal/workflow/statusupdate"
	"github.com/beelinewf/wf-engine/internal/workflow/transport"
	"github.com/beelinewf/wf-engine/pkg/configuration"
	_ "github.com/beelinewf/wf-engine/pkg/customnode/all"
	"github.com/beelinewf/wf-engine/pkg/database"
	"github.com/beelinewf/wf-engine/pkg/envsnapshot"
	"github.com/sirupsen/logrus"
)

const (
	// shutdownBudget is the single worst-case grace for the whole
	// shutdown sequence. The HTTP drain and every deferred stage share
	// one clock, so sequential stages split the budget instead of each
	// taking a full timeout.
	shutdownBudget = 25 * time.Second
	// defaultTerminationGrace is the orchestrator grace the budget must
	// fit: Kubernetes sends SIGKILL after terminationGracePeriodSeconds
	// (30s unless the manifest overrides it).
	defaultTerminationGrace = 30 * time.Second
	// statusLoaderCacheSize bounds the memoized status_update configs
	// served to the delivery dispatcher.
	statusLoaderCacheSize = 1024
	// snapshotTimeout bounds one metrics snapshot query.
	snapshotTimeout = 5 * time.Second
)

// sharedShutdown starts one shutdown clock on first use and hands every
// stage a context on that clock. The first stage to shut down starts the
// budget; later stages get whatever remains.
type sharedShutdown struct {
	budget time.Duration
	once   sync.Once
	ctx    context.Context
	cancel context.CancelFunc
}

func newSharedShutdown(budget time.Duration) *sharedShutdown {
	return &sharedShutdown{budget: budget}
}

func (s *sharedShutdown) context() context.Context {
	s.once.Do(func() {
		s.ctx, s.cancel = context.WithTimeout(context.Background(), s.budget)
	})
	return s.ctx
}

func (s *sharedShutdown) stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

var errMetricsSnapshot = errors.New("metrics snapshot not implemented")

func main() {
	configPath := flag.String("config", "", "path to config file (default: config.yaml in working directory)")
	flag.Parse()

	logger, err := NewLogger("info")
	if err != nil {
		logrus.Fatal(err)
	}

	cfg, err := configuration.Load(configuration.WithConfigFile(*configPath))
	if err != nil {
		logger.Fatalf("load configuration: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, logger); err != nil {
		logger.Fatalf("run: %v", err)
	}
	logger.Info("app stopped cleanly")
}

// warnAboutCatalog reports configured role permissions that will never take
// effect. An unknown action is not rejected: the catalog is data, and a
// deployment may legitimately grant an action a newer build gates on. But a
// typo silently grants nothing, which is exactly the failure an operator
// cannot see from the outside, so it is worth a boot-time line. A role name
// that is blank once trimmed names nobody at all.
func warnAboutCatalog(rolePermissions map[string][]string, logger *logrus.Logger) {
	known := make(map[string]bool, len(auth.KnownActions()))
	for _, action := range auth.KnownActions() {
		known[action] = true
	}
	roles := make([]string, 0, len(rolePermissions))
	for role := range rolePermissions {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		if strings.TrimSpace(role) == "" {
			logger.Warnf("auth: a configured role name is blank and grants nothing")
			continue
		}
		for _, action := range rolePermissions[role] {
			action = strings.TrimSpace(action)
			switch {
			case action == "":
				logger.Warnf("auth: role %q has a blank permission, which grants nothing", role)
			case !known[action] && action != auth.WildcardAction:
				logger.Warnf("auth: role %q grants unknown action %q; no route gates on it", role, action)
			}
		}
	}
	// A seed with no roles:write holder is legal — a first boot may simply
	// not grant it yet — but roles then cannot be managed over the API, so
	// it gets a line rather than failing the boot. The API token bypasses
	// every gate and stays the recovery path.
	if !auth.HasRolesWriteHolder(rolePermissions) {
		logger.Warnf("auth: no role grants roles:write, so roles cannot be managed over the API until one does")
	}
}

// loadRoleCatalog decides what fills the shared role catalog at startup.
// While the role tables are empty, configuration seeds them; otherwise the
// database wins and configuration is ignored for roles.
func loadRoleCatalog(ctx context.Context, roles repository.RoleRepository, authSvc service.AuthService, rolePermissions map[string][]string, logger *logrus.Logger) error {
	existing, _, err := roles.List(ctx)
	if err != nil {
		return fmt.Errorf("list roles: %w", err)
	}
	if len(existing) == 0 {
		warnAboutCatalog(rolePermissions, logger)
		// The catalog is upserted and its stale permissions pruned, but no
		// role row is ever deleted: a role dropped from the config keeps
		// its rows so an in-flight token still resolves it.
		if err := authSvc.SeedRoles(ctx); err != nil {
			return fmt.Errorf("seed roles: %w", err)
		}
		return nil
	}
	if err := authSvc.RefreshCatalog(ctx); err != nil {
		return fmt.Errorf("refresh catalog: %w", err)
	}
	return nil
}

// catalogRefreshInterval bounds how stale one replica's in-memory catalog
// can go after another replica writes a role.
const catalogRefreshInterval = 30 * time.Second

// refreshCatalogLoop reloads the shared catalog on an interval so role
// writes made through other replicas take effect locally. A failed refresh
// warns and retries on the next tick; the loop ends with the context.
func refreshCatalogLoop(ctx context.Context, authSvc service.AuthService, logger *logrus.Logger) {
	ticker := time.NewTicker(catalogRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := authSvc.RefreshCatalog(ctx); err != nil && ctx.Err() == nil {
			logger.Warnf("catalog refresh: %v", err)
		}
	}
}

// checkAuthWiring refuses to boot an authentication setup that cannot name
// the caller it just authenticated.
//
// OIDC resolves a verified token onto a users row just in time, keyed by
// (subject, issuer); the human rows get their id from that upsert, not from
// the configured actor. The dangerous case a blank audit actor creates is
// narrower but still silent: the API-token service principal resolves to an
// empty user id, as do the engine background effects that act as the
// configured actor, so their created_by/updated_by rows are written empty
// and the work is attributed to nobody. The failure is silent, so it is
// cheaper to refuse the boot. Without OIDC the same misconfiguration only
// affects the API-token service principal, which is optional, so it is left
// to the configured default.
func checkAuthWiring(verifier auth.Verifier, authSvc service.AuthService, actor string) error {
	if verifier == nil {
		return nil
	}
	if authSvc == nil {
		return errors.New("token auth enabled without an identity resolver: the auth service is required")
	}
	if strings.TrimSpace(actor) == "" {
		return errors.New("token auth enabled without a system user id: system.user_id is required")
	}
	return nil
}

// databaseOptions builds the production pool settings from the loaded
// configuration. It starts from database.DefaultOptions so the pool ceiling
// is never left at the database/sql zero values (unbounded); the DSN, the
// four pool knobs, and the connect timeout come from config, which Load
// already defaulted and validated.
func databaseOptions(cfg *configuration.Config) database.Options {
	opts := database.DefaultOptions()
	opts.DSN = cfg.Infra.PostgreSQL.DSN
	opts.MaxOpenConns = cfg.Infra.PostgreSQL.MaxOpenConns
	opts.MaxIdleConns = cfg.Infra.PostgreSQL.MaxIdleConns
	opts.ConnMaxLifetime = cfg.Infra.PostgreSQL.ConnMaxLifetime
	opts.ConnMaxIdleTime = cfg.Infra.PostgreSQL.ConnMaxIdleTime
	opts.ConnectTimeout = cfg.Infra.PostgreSQL.ConnectTimeout
	return opts
}

// executorLimits maps engine config to executor limits. It builds the one
// shared HTTP bulkhead from the configured caps so nodes, pollers, the
// status publisher, and custom nodes are all admitted process-wide.
func executorLimits(cfg *configuration.Config) executor.Limits {
	maxRedirects := cfg.Engine.MaxRedirects
	if maxRedirects <= 0 {
		maxRedirects = 5
	}
	return executor.Limits{
		HTTPAllowlist:          cfg.Engine.HTTPAllowlist,
		ExecAllowlist:          cfg.Engine.ExecAllowlist,
		MaxOutputBytes:         cfg.Engine.MaxOutputBytes,
		MaxRedirects:           maxRedirects,
		HTTPBulkhead:           executor.NewHTTPBulkhead(cfg.Engine.HTTPMaxInFlight, cfg.Engine.HTTPMaxInFlightPerHost),
		HTTPMaxInFlight:        cfg.Engine.HTTPMaxInFlight,
		HTTPMaxInFlightPerHost: cfg.Engine.HTTPMaxInFlightPerHost,
	}
}

// engineDispatcherOptions maps engine and worker config to engine dispatcher options.
func engineDispatcherOptions(cfg *configuration.Config, limits model.Limits) engine.DispatcherOptions {
	return engine.DispatcherOptions{
		PollInterval: cfg.Engine.PollInterval,
		Lease:        limits.LeaseDuration,
		BatchSize:    limits.ClaimBatchSize,
		PoolSize:     cfg.Worker.Pool.Size,
		Heartbeat:    cfg.Engine.HeartbeatInterval,
	}
}

// statusDispatcherOptions maps worker and engine config to status dispatcher options.
func statusDispatcherOptions(cfg *configuration.Config, limits model.Limits) statusupdate.DispatcherOptions {
	return statusupdate.DispatcherOptions{
		PollInterval: cfg.Engine.PollInterval,
		Lease:        limits.LeaseDuration,
		BatchSize:    limits.ClaimBatchSize,
		PoolSize:     cfg.Worker.StatusPool.Size,
	}
}

// newHTTPServer builds the API server from config. ReadHeaderTimeout closes
// slow-header connections and IdleTimeout reaps idle keep-alives;
// WriteTimeout stays unset so SSE streams are never cut.
func newHTTPServer(cfg *configuration.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.Infra.HTTP.Host,
		Handler:           handler,
		ReadHeaderTimeout: cfg.Infra.HTTP.ReadHeaderTimeout,
		IdleTimeout:       cfg.Infra.HTTP.IdleTimeout,
	}
}

// metricsRefresh returns the per-scrape live-gauge updater: broker consumer
// liveness and bulkhead occupancy are in-memory and cheap, so they refresh
// on every scrape with no TTL staleness and no goroutine.
func metricsRefresh(rec *metrics.Recorder, monitors map[string]handler.ConsumerMonitor, bulkhead *executor.HTTPBulkhead) func() {
	return func() {
		for transport, m := range monitors {
			rec.SetConsumerHealthy(transport, m.Healthy())
		}
		rec.SetBulkheadInflight(float64(bulkhead.GlobalInflight()))
	}
}

// run starts the HTTP server and the dispatcher, and blocks until ctx is
// cancelled, then shuts down gracefully. It returns nil on a clean shutdown.
func run(ctx context.Context, cfg *configuration.Config, logger *logrus.Logger) error {
	// Env snapshot deny adjustments are global state read by every snapshot
	// and render, so they are installed once here before any workflow runs.
	envsnapshot.SetOverride(cfg.Engine.EnvDenyExtra, cfg.Engine.EnvAllowExceptions)

	// The secret at-rest cipher is likewise global to the repository
	// layer. Configuration already validated the key; installing it here,
	// before any repository is used, means every secret write below is
	// sealed and every read is opened transparently.
	if err := repository.InstallSecretEncryption(cfg.Secrets.EncryptionKey); err != nil {
		return fmt.Errorf("secret encryption: %w", err)
	}

	db, err := database.New(databaseOptions(cfg))
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("database pool: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()
	// The shared shutdown clock is released last: this defer is
	// registered before every stage defer, so it runs after them.
	shutdown := newSharedShutdown(shutdownBudget)
	defer shutdown.stop()
	logger.Info("database connected")

	actor := cfg.System.UserID
	if actor == "" {
		actor = "11111111-1111-7111-8111-111111111111"
	}
	sysUser := model.User{ID: actor, Name: cfg.System.Name, Email: cfg.System.Email, IsActive: true}
	if sysUser.Name == "" {
		sysUser.Name = "system"
	}
	if sysUser.Email == "" {
		sysUser.Email = "system@localhost"
	}
	verifiedAt := time.Now().UTC()
	sysUser.EmailVerifiedAt = &verifiedAt
	if err := repository.UpsertSystemUser(ctx, db, sysUser); err != nil {
		return fmt.Errorf("seed system user: %w", err)
	}

	nodeDefs := repository.NewNodeDefinitionRepository(db)
	wfDefs := repository.NewWorkflowDefinitionRepository(db)
	secrets := repository.NewSecretRepository(db)
	// Boot probe: a wrong key must fail here, not at the first instance
	// creation. GetAll is the one read that surfaces undecryptable
	// values as an error, and the stored-context spot-check covers the
	// case where the secrets table itself has nothing sealed to open.
	if _, err := secrets.GetAll(ctx); err != nil {
		return fmt.Errorf("secret encryption probe: %w", err)
	}
	if err := repository.VerifySecretEncryptionReadable(ctx, db); err != nil {
		return fmt.Errorf("secret encryption probe: %w", err)
	}
	forms := repository.NewFormRepository(db)
	users := repository.NewUserRepository(db)
	roles := repository.NewRoleRepository(db)

	// Authentication is configured, not discovered: the OIDC provider is
	// contacted at startup so a wrong issuer or client id fails the boot
	// rather than every request. With OIDC off, the nil verifier leaves the
	// API-token service principal (or an unauthenticated deployment) as-is.
	var oidcVerifier auth.Verifier
	if cfg.Auth.OIDC.Enabled {
		oidcAuth, err := auth.NewOIDCAuthenticator(ctx, cfg.Auth.OIDC)
		if err != nil {
			return fmt.Errorf("oidc: %w", err)
		}
		oidcVerifier = oidcAuth
		logger.Infof("oidc enabled for issuer %s", cfg.Auth.OIDC.Issuer)
	}
	// Local auth mints the API's own JWTs for customers without a
	// provider. The constructor re-checks the secret and TTL the
	// configuration already validated, so a hand-built config fails the
	// boot here rather than the first login.
	var localAuth *auth.LocalAuthenticator
	var mailSender mail.Sender
	if cfg.Auth.Local.Enabled {
		var err error
		localAuth, err = auth.NewLocalAuthenticator(cfg.Auth.Local.JWTSecret, cfg.Auth.Local.Issuer, cfg.Auth.Local.ClientID, cfg.Auth.Local.TokenTTL)
		if err != nil {
			return fmt.Errorf("local auth: %w", err)
		}
		if cfg.Auth.Local.EmailVerification.Enabled {
			smtpCfg := cfg.Auth.Local.SMTP
			mailSender, err = mail.NewSender(smtpCfg.Host, smtpCfg.Port, smtpCfg.Username, smtpCfg.Password, smtpCfg.From, smtpCfg.UseTLS, smtpCfg.TemplateDir)
			if err != nil {
				return fmt.Errorf("local auth mail: %w", err)
			}
		}
		logger.Infof("local auth enabled for issuer %s", cfg.Auth.Local.Issuer)
	}
	// One verifier for both token kinds: the chain routes by the token's
	// issuer before doing any crypto, so the middleware below never learns
	// which side minted the Bearer [REDACTED]
	verifier := auth.NewChainVerifier(oidcVerifier, localAuth)
	catalog := auth.CatalogFromConfig(cfg.Auth)
	var svcOpts []service.AuthServiceOption
	if cfg.Auth.Local.Enabled {
		svcOpts = append(svcOpts, service.WithLocalAuth(service.LocalAuthDeps{
			Issuer:               localAuth,
			Mail:                 mailSender,
			AllowRegistration:    cfg.Auth.Local.AllowRegistration,
			DefaultRole:          cfg.Auth.Local.DefaultRole,
			VerificationEnabled:  cfg.Auth.Local.EmailVerification.Enabled,
			VerificationTokenTTL: cfg.Auth.Local.EmailVerification.TokenTTL,
			ResendCooldown:       cfg.Auth.Local.EmailVerification.ResendCooldown,
			PublicBaseURL:        cfg.Auth.Local.PublicBaseURL,
		}))
	}
	authSvc := service.NewAuthService(users, roles, catalog, actor, svcOpts...)
	if err := checkAuthWiring(verifier, authSvc, actor); err != nil {
		return err
	}
	// Configuration seeds the role tables only while they are empty; once
	// seeded, the database is the source of truth and the shared catalog
	// loads from it.
	if err := loadRoleCatalog(ctx, roles, authSvc, cfg.Auth.RolePermissions, logger); err != nil {
		return err
	}
	// Another replica's role writes land in the shared tables; the ticker
	// bounds how stale this replica's in-memory catalog can go.
	go refreshCatalogLoop(ctx, authSvc, logger)

	leanOpts := model.LeanOptions{
		LeanContextDefault: cfg.Engine.LeanContextDefault,
		AnchorEvery:        cfg.Engine.LeanAnchorEvery,
		ReplayMax:          cfg.Engine.LeanReplayMax,
	}
	instances := repository.NewInstanceRepositoryWithOptions(db, leanOpts)

	nodeLimits := model.NodeLimits{
		DefaultTimeout:   cfg.Engine.DefaultNodeTimeout,
		MaxTimeout:       cfg.Engine.MaxNodeTimeout,
		ConditionTimeout: cfg.Engine.ConditionTimeout,
		Parallel: model.ParallelLimits{
			MaxDepth:               cfg.Engine.Parallel.MaxDepth,
			MaxBranchesPerParallel: cfg.Engine.Parallel.MaxBranchesPerParallel,
		},
	}
	if nodeLimits.DefaultTimeout <= 0 {
		nodeLimits.DefaultTimeout = 30 * time.Second
	}
	if nodeLimits.MaxTimeout <= 0 {
		nodeLimits.MaxTimeout = 30 * time.Second
	}
	if nodeLimits.ConditionTimeout <= 0 {
		nodeLimits.ConditionTimeout = 5 * time.Second
	}

	limits := model.DefaultLimits()
	if cfg.Engine.MaxPerNodeExecutions > 0 {
		limits.MaxPerNodeExecutions = cfg.Engine.MaxPerNodeExecutions
	}
	if cfg.Engine.MaxTotalExecutions > 0 {
		limits.MaxTotalExecutions = cfg.Engine.MaxTotalExecutions
	}
	if cfg.Engine.LeaseDuration > 0 {
		limits.LeaseDuration = cfg.Engine.LeaseDuration
	}
	if cfg.Engine.ClaimBatchSize > 0 {
		limits.ClaimBatchSize = cfg.Engine.ClaimBatchSize
	}
	if cfg.Engine.Parallel.MaxActiveBranchesPerInstance > 0 {
		limits.MaxActiveBranchesPerInstance = cfg.Engine.Parallel.MaxActiveBranchesPerInstance
	}
	if cfg.Engine.Parallel.MaxDepth > 0 {
		limits.MaxParallelDepth = cfg.Engine.Parallel.MaxDepth
	}

	execLimits := executorLimits(cfg)

	nodeSvc := service.NewNodeDefinitionService(nodeDefs, nodeLimits, actor, forms)
	wfSvc := service.NewWorkflowDefinitionService(wfDefs, nodeDefs, nodeLimits, actor, forms)
	secretSvc := service.NewSecretService(secrets)
	formSvc := service.NewFormService(forms, wfSvc)

	// Optional broker clients. They exist only when their DSN is configured;
	// a configured but unreachable broker fails startup. The close defers are
	// registered before the dispatcher/consumer defers below so shutdown
	// stops consumers and dispatchers before the connections close. The
	// interface-typed vars stay nil when the broker is absent.
	var redisClient *transport.RedisClient
	var redisPub transport.RedisPublisher
	if dsn := cfg.Infra.Redis.DSN; dsn != "" {
		rc, err := transport.NewRedisClient(ctx, dsn)
		if err != nil {
			return fmt.Errorf("redis: %w", err)
		}
		redisClient = rc
		redisPub = rc
		defer func() { _ = redisClient.Close() }()
		logger.Info("redis connected")
	}
	var rabbitClient *transport.RabbitClient
	var rabbitPub transport.RabbitPublisher
	if dsn := cfg.Infra.RabbitMQ.DSN; dsn != "" {
		rc, err := transport.NewRabbitClient(ctx, dsn,
			cfg.Infra.RabbitMQ.InputQueue, cfg.Infra.RabbitMQ.OutputQueue, cfg.Infra.RabbitMQ.StatusQueue)
		if err != nil {
			return fmt.Errorf("rabbitmq: %w", err)
		}
		rabbitClient = rc
		rabbitPub = rc
		defer func() { _ = rabbitClient.Close() }()
		logger.Info("rabbitmq connected")
	}

	// Optional object storage for multipart input uploads. The client is
	// built once here; a nil store means storage is disabled and multipart
	// deliveries fail at delivery time.
	var objStore objectstore.Store
	if cfg.Infra.Storage.Enabled() {
		st, err := objectstore.NewMinioStore(
			cfg.Infra.Storage.Endpoint,
			cfg.Infra.Storage.Region,
			cfg.Infra.Storage.AccessKey,
			cfg.Infra.Storage.SecretKey,
			cfg.Infra.Storage.UseSSL,
		)
		if err != nil {
			return fmt.Errorf("storage: %w", err)
		}
		objStore = st
		logger.Info("object storage configured")
	}

	hookRunner := executor.NewHookRunner(nil)
	executors := executor.NewExecutors(execLimits, nil, executor.Dependencies{
		Output:       executor.NewBrokerOutputPublisher(redisPub, rabbitPub, cfg.Infra.RabbitMQ.OutputQueue),
		RedisPoller:  redisClient,
		RabbitPoller: rabbitClient,
	})
	loader := func(ctx context.Context, instanceID string) (*model.WorkflowContent, error) {
		inst, err := instances.GetByID(ctx, instanceID)
		if err != nil {
			return nil, err
		}
		return wfSvc.Resolve(ctx, inst.WorkflowDefinitionID)
	}
	eng := engine.NewEngine(instances, repository.NewParallelRepository(db), executors, hookRunner, limits, loader, actor, leanOpts)
	instSvc := service.NewInstanceServiceWithMultipart(
		instances,
		repository.NewParallelRepository(db),
		wfDefs,
		secrets,
		forms,
		wfSvc,
		&executor.InputExecutor{},
		hookRunner,
		actor,
		nodeLimits,
		eng,
		leanOpts,
		catalog,
		service.MultipartDeps{
			Store: objStore,
			Limits: multipart.Limits{
				MaxFiles:      cfg.Engine.Multipart.MaxFiles,
				MaxTotalBytes: cfg.Engine.Multipart.MaxTotalBytes,
				MaxFileBytes:  cfg.Engine.Multipart.MaxFileBytes,
				MaxTextBytes:  cfg.Engine.Multipart.MaxTextBytes,
			},
		},
	)
	statsSvc := service.NewStatisticsService(instances)
	hostname, _ := os.Hostname()

	// Broker input consumers deliver payloads to waiting input nodes whose
	// channel matches the transport. They stop on ctx cancellation; the
	// consumer wait defer is registered before the dispatcher defers so
	// shutdown waits for in-flight deliveries before closing the brokers.
	var consumers sync.WaitGroup
	monitors := make(map[string]handler.ConsumerMonitor)
	retry := inputtransport.CappedOptions{
		MaxAttempts:    cfg.Engine.ConsumerRetry.MaxAttempts,
		InitialBackoff: cfg.Engine.ConsumerRetry.InitialBackoff,
		MaxBackoff:     cfg.Engine.ConsumerRetry.MaxBackoff,
		MinHealthyRun:  cfg.Engine.ConsumerRetry.MinHealthyRun,
		Sleep:          inputtransport.SleepInterruptible,
		Now:            time.Now,
	}
	if redisClient != nil {
		redisDSN := cfg.Infra.Redis.DSN
		redisStatus := inputtransport.NewConsumerStatus("redis")
		monitors["redis"] = redisStatus
		consumers.Add(1)
		go func() {
			defer consumers.Done()
			inputtransport.SuperviseCapped(ctx, redisStatus, func(runCtx context.Context) error {
				// A dedicated connection per attempt: the shared
				// publisher client is untouched by retry churn, and a
				// dead client is discarded instead of reused.
				rc, err := transport.NewRedisClient(runCtx, redisDSN)
				if err != nil {
					return err
				}
				defer func() { _ = rc.Close() }()
				return inputtransport.NewRedisInput(rc, instSvc).Run(runCtx)
			}, retry)
		}()
		logger.Info("redis input consumer started")
	}
	if rabbitClient != nil {
		rabbitDSN := cfg.Infra.RabbitMQ.DSN
		inputQueue := cfg.Infra.RabbitMQ.InputQueue
		outputQueue := cfg.Infra.RabbitMQ.OutputQueue
		statusQueue := cfg.Infra.RabbitMQ.StatusQueue
		consumerTag := "input-" + hostname
		rabbitStatus := inputtransport.NewConsumerStatus("rabbitmq")
		monitors["rabbitmq"] = rabbitStatus
		consumers.Add(1)
		go func() {
			defer consumers.Done()
			inputtransport.SuperviseCapped(ctx, rabbitStatus, func(runCtx context.Context) error {
				// amqp never heals a broken connection, so each
				// attempt dials a fresh client and discards it.
				rc, err := transport.NewRabbitClient(runCtx, rabbitDSN, inputQueue, outputQueue, statusQueue)
				if err != nil {
					return err
				}
				defer func() { _ = rc.Close() }()
				return inputtransport.NewRabbitInput(rc, instSvc, consumerTag).Run(runCtx)
			}, retry)
		}()
		logger.Info("rabbitmq input consumer started")
	}
	defer func() {
		stopCtx := shutdown.context()
		done := make(chan struct{})
		go func() {
			consumers.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-stopCtx.Done():
			logger.Warn("broker input consumers did not stop in time")
		}
	}()

	dispatcher, err := engine.NewDispatcher(ctx, eng, instances, repository.NewParallelRepository(db), "dispatcher-"+hostname, engineDispatcherOptions(cfg, limits))
	if err != nil {
		return fmt.Errorf("dispatcher: %w", err)
	}
	dispatcher.Run()
	defer func() {
		if err := dispatcher.Shutdown(shutdown.context()); err != nil {
			logger.Warnf("dispatcher shutdown: %v", err)
		}
	}()

	// Status-update dispatcher: drains the transactional outbox, delivering
	// notifications in strict per-instance and per-transport order through
	// the configured transport publishers (http always; redis and rabbitmq
	// when their clients exist).
	statusUpdates := repository.NewStatusUpdateRepository(db)

	// Metrics registry with snapshot sources. DB pool stats read
	// sqlDB.Stats in memory; repository snapshots refresh at most once
	// per TTL inside the collector, so scrape load stays flat. Narrow
	// interface assertions keep the repository interfaces untouched:
	// a repo that does not implement a snapshot simply leaves its
	// gauge at zero.
	recorder := metrics.New(metrics.Sources{
		DBStats: func() (metrics.DBPoolStats, error) {
			s := sqlDB.Stats()
			return metrics.DBPoolStats{
				OpenConnections: s.OpenConnections,
				InUse:           s.InUse,
				WaitCount:       s.WaitCount,
				WaitDuration:    s.WaitDuration,
			}, nil
		},
		InstancesActive: func() (map[string]int64, error) {
			type counter interface {
				CountActiveByStatus(context.Context) (map[string]int64, error)
			}
			s, ok := instances.(counter)
			if !ok {
				return nil, errMetricsSnapshot
			}
			ctx, cancel := context.WithTimeout(context.Background(), snapshotTimeout)
			defer cancel()
			return s.CountActiveByStatus(ctx)
		},
		DispatcherStats: func() (int64, float64, error) {
			type stats interface {
				PendingDispatchStats(context.Context) (int64, float64, error)
			}
			s, ok := instances.(stats)
			if !ok {
				return 0, 0, errMetricsSnapshot
			}
			ctx, cancel := context.WithTimeout(context.Background(), snapshotTimeout)
			defer cancel()
			return s.PendingDispatchStats(ctx)
		},
		StatusOutboxDepth: func() (int64, error) {
			type depth interface {
				OutboxDepth(context.Context) (int64, error)
			}
			s, ok := statusUpdates.(depth)
			if !ok {
				return 0, errMetricsSnapshot
			}
			ctx, cancel := context.WithTimeout(context.Background(), snapshotTimeout)
			defer cancel()
			return s.OutboxDepth(ctx)
		},
	})
	eng.SetMetrics(recorder)
	dispatcher.SetMetrics(recorder)
	if s, ok := instSvc.(interface{ SetMetrics(*metrics.Recorder) }); ok {
		s.SetMetrics(recorder)
	}
	statusLoader := func(ctx context.Context, definitionID string) (*model.StatusUpdateConfig, error) {
		def, err := wfDefs.GetByID(ctx, definitionID)
		if err != nil {
			return nil, err
		}
		return model.ParseStatusUpdate(def.Content)
	}
	// Definitions are immutable: parse each definition's status_update
	// block once and serve every later delivery from the cache.
	statusLoader = statusupdate.CachedLoader(statusLoader, statusLoaderCacheSize)
	statusPublishers := map[string]statusupdate.Publisher{
		model.StatusUpdateTransportHTTP: statusupdate.NewHTTPPublisher(executor.NewHTTPExecutor(execLimits), 0),
	}
	if redisPub != nil {
		statusPublishers[model.StatusUpdateTransportRedis] = statusupdate.NewRedisPublisher(redisPub)
	}
	if rabbitPub != nil {
		statusPublishers[model.StatusUpdateTransportRabbitMQ] = statusupdate.NewRabbitPublisher(rabbitPub, cfg.Infra.RabbitMQ.StatusQueue)
	}
	statusDispatcher, err := statusupdate.NewDispatcher(ctx, statusUpdates, statusLoader,
		statusPublishers, "status-"+hostname, statusDispatcherOptions(cfg, limits))
	if err != nil {
		return fmt.Errorf("status dispatcher: %w", err)
	}
	statusDispatcher.SetMetrics(recorder)
	statusDispatcher.Run()
	defer func() {
		if err := statusDispatcher.Shutdown(shutdown.context()); err != nil {
			logger.Warnf("status dispatcher shutdown: %v", err)
		}
	}()

	// Cron scheduler: creates an instance of the target definition on
	// every tick of each enabled schedule. All enabled replicas fire and
	// the fire claim keeps each tick to one instance (see the scheduler
	// package); onChange refreshes the local entries after every
	// mutation, and other replicas catch up on the refresh interval.
	schedRepo := repository.NewScheduleRepository(db)
	var onScheduleChange func()
	if cfg.Scheduler.Enabled {
		cronScheduler := scheduler.New(ctx, schedRepo, instSvc, "scheduler-"+hostname, scheduler.SchedulerOptions{
			RefreshInterval: cfg.Scheduler.RefreshInterval,
		})
		cronScheduler.SetMetrics(recorder)
		onScheduleChange = cronScheduler.Refresh
		if err := cronScheduler.Run(); err != nil {
			return fmt.Errorf("scheduler: %w", err)
		}
		defer func() {
			if err := cronScheduler.Shutdown(shutdown.context()); err != nil {
				logger.Warnf("scheduler shutdown: %v", err)
			}
		}()
	}
	schedSvc := service.NewScheduleService(schedRepo, wfDefs, actor, onScheduleChange)

	router := handler.NewRouter(handler.Deps{
		Health:              handler.NewHealth(sqlDB).WithConsumers(monitors),
		NodeDefinitions:     nodeSvc,
		WorkflowDefinitions: wfSvc,
		Secrets:             secretSvc,
		Forms:               formSvc,
		Instances:           instSvc,
		Schedules:           schedSvc,
		Statistics:          statsSvc,
		Auth:                authSvc,
		SwaggerEnabled:      cfg.Infra.HTTP.SwaggerEnabled,
		MetricsEnabled:      cfg.Infra.HTTP.MetricsEnabled,
		MaxBodyBytes:        cfg.Infra.HTTP.MaxBodyBytes,
		Metrics:             recorder,
		MetricsRefresh:      metricsRefresh(recorder, monitors, execLimits.HTTPBulkhead),
		AuthSettings:        cfg.Auth,
		CORS:                cfg.Infra.HTTP.CORS,
		OIDC:                verifier,
		Catalog:             catalog,
		SystemUserID:        actor,
	})

	srv := newHTTPServer(cfg, router)

	errCh := make(chan error, 1)
	go func() {
		logger.Infof("http server listening on %s", cfg.Infra.HTTP.Host)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		if err := srv.Shutdown(shutdown.context()); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		logger.Info("http server stopped")
		return nil
	case err := <-errCh:
		return fmt.Errorf("http serve: %w", err)
	}
}

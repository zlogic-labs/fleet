package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/apiserver"
	"github.com/zlogic-labs/fleet/core/internal/detail/clickhouse"
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/internal/registry"
	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/logconf"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fleet-apiserver:", err)
		os.Exit(1)
	}
}

type flags struct {
	listen         string
	dataDir        string
	hubURL         string
	hubToken       string
	adminTokens    tokens
	pullConc       int
	fileConc       int
	allowedOrigins []string
	dev            bool
	devDelay       time.Duration
	logLevel       string
	databaseURL    string
	migrate        bool
	showVer        bool
}

func run() error {
	var f flags
	fs := flag.NewFlagSet("fleet-apiserver", flag.ExitOnError)
	fs.StringVar(&f.listen, "listen", envOr("FLEET_APISERVER_LISTEN", ":8081"),
		"address to serve the management API on")
	fs.StringVar(&f.databaseURL, "database", envOr("FLEET_DATABASE_URL", ""),
		"PostgreSQL URL; without one the control plane runs but has no tenancy")
	fs.Var(&f.adminTokens, "admin-token",
		"bearer token guarding every management route; required unless bound to loopback. "+
			"Repeatable, so one person's departure is a revocation rather than a rotation that "+
			"breaks everyone else. FLEET_ADMIN_TOKEN may carry a comma-separated list. "+
			"The gateway and the console must be given one of the same values")
	// Defaults to true, unlike the same flag on fleet-gateway, and the difference
	// is deliberate rather than an oversight: this is the management process, so
	// it is the one an operator starts by hand and the one that owns the
	// database in a single-node deployment. The gateway is on the request path,
	// runs several replicas in production, and should not need DDL privileges
	// to serve a completion — so it defaults to off. Same variable, two
	// defaults, one stated reason.
	fs.BoolVar(&f.migrate, "database-migrate", envBool("FLEET_DATABASE_MIGRATE", true),
		"create the schema at startup; IF NOT EXISTS only creates what is absent. "+
			"Defaults on here and off in fleet-gateway, which should not hold DDL privileges")
	fs.StringVar(&f.dataDir, "data", envOr("FLEET_DATA_DIR", "./fleet-data"),
		"directory for the local object store, used when no S3 endpoint is configured")
	fs.StringVar(&f.hubURL, "hub", envOr("FLEET_HUB_URL", "https://huggingface.co"),
		"model repository base URL")
	fs.StringVar(&f.hubToken, "hub-token", envOr("FLEET_HUB_TOKEN", ""),
		"token for gated repositories; a per-request token overrides it")
	fs.IntVar(&f.pullConc, "pull-concurrency", 2,
		"how many pulls may run at once")
	fs.IntVar(&f.fileConc, "file-concurrency", 3,
		"how many files within one pull may download at once")
	// Seeded from the environment because cors.go documents
	// FLEET_ALLOWED_ORIGINS and nothing read it: the flag was a Func, so it had
	// no envOr default, and an operator following the documentation got
	// loopback-only CORS. Passing the flag still wins, since Func runs after
	// this assignment.
	f.allowedOrigins = apiserver.CorsOrigins(envOr("FLEET_ALLOWED_ORIGINS", ""))
	fs.Func("allowed-origins", "comma-separated CORS origins; empty allows loopback only",
		func(v string) error {
			f.allowedOrigins = apiserver.CorsOrigins(v)
			return nil
		})
	fs.BoolVar(&f.dev, "dev", false,
		"use a synthetic model repository instead of the real Hub; nothing downloaded is loadable")
	fs.DurationVar(&f.devDelay, "dev-file-delay", 0,
		"in --dev, pause this long per file, so a pull is slow enough to watch or cancel")
	fs.StringVar(&f.logLevel, "log-level", "info", "debug, info, warn or error")
	fs.BoolVar(&f.showVer, "version", false, "print the version and exit")
	f.adminTokens.seed("FLEET_ADMIN_TOKEN")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if f.showVer {
		fmt.Println("fleet-apiserver", version)
		return nil
	}

	log := logconf.New(f.logLevel, logconf.Text)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := openStore()
	if err != nil {
		return err
	}
	if err := store.EnsureBucket(ctx); err != nil {
		return fmt.Errorf("preparing storage: %w", err)
	}

	h, note := pickHub(f)
	log.Info("control plane starting",
		"version", version, "listen", f.listen, "hub", note,
		"pull_concurrency", f.pullConc)

	// No URL means no tenancy. Everything else in the control plane works
	// without a database, and refusing to start would make PostgreSQL a
	// prerequisite for trying Fleet.
	var db *sqlstore.DB
	if f.databaseURL != "" {
		db, err = sqlstore.Open(ctx, sqlstore.Config{
			URL: f.databaseURL, ConnectTimeout: 10 * time.Second,
		})
		if err != nil {
			return err
		}
		defer db.Close()
		if f.migrate {
			if err := db.Migrate(ctx); err != nil {
				return err
			}
		}
	} else {
		log.Warn("no --database; tenants, keys and budgets are unavailable")
	}

	var policySource *sqlstore.PolicySource
	var keyStore *sqlstore.KeyStore
	var quotaStore *sqlstore.Quota
	var costStore *sqlstore.CostStore
	var priceStore *sqlstore.PriceStore
	if db != nil {
		policySource = sqlstore.NewPolicySource(db, ratelimit.Policy{})
		// The same TTL the gateway uses, and deliberately so: this process mints and
		// revokes keys, so a stale entry here is an operator who deleted a leaked
		// credential and finds it still working. The 5 minutes this used to carry
		// was ten times the staleness budget NewTTLCache documents, in the one
		// binary where a long cache is a security problem rather than a saving.
		// The limit is left to NewTTLCache's own default rather than restated.
		keyStore = sqlstore.NewKeyStore(db, sqlstore.NewTTLCache(30*time.Second, 0))
		quotaStore = sqlstore.NewQuota(db)
		costStore = sqlstore.NewCostStore(db)
		// This store only declares books; the gateway has its own with a cache
		// and a refresh cadence. Sharing one would make a price change depend on
		// which process was asked.
		priceStore = sqlstore.NewPriceStore(db, time.Minute)
	}

	srv, err := apiserver.NewServer(apiserver.Config{
		Listen:          f.listen,
		Blobs:           store,
		Hub:             h,
		PullConcurrency: f.pullConc,
		FileConcurrency: f.fileConc,
		AllowedOrigins:  f.allowedOrigins,
		AdminTokens:     f.adminTokens,
		DB:              db,
		Policies:        policySource,
		Keys:            keyStore,
		Quota:           quotaStore,
		Cost:            costStore,
		Prices:          priceStore,
		Detail:          clickhouse.FromEnv(),
	}, registry.NewMemory(), log)
	if err != nil {
		return err
	}
	defer srv.Close()

	httpSrv := &http.Server{
		Addr:    f.listen,
		Handler: srv.Handler(),
		// No WriteTimeout: inventory reports and pull status are small, but a
		// hung client must not be able to hold a connection forever, and
		// ReadHeaderTimeout is the one that actually matters here.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", f.listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down; in-flight pulls are allowed to finish")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		return nil
	}
}

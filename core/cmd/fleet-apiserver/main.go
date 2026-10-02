package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/apiserver"
	"github.com/zlogic-labs/fleet/core/internal/blobstore"
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/internal/hub"
	"github.com/zlogic-labs/fleet/core/internal/registry"
	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
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
	fs.BoolVar(&f.migrate, "database-migrate", envBool("FLEET_DATABASE_MIGRATE", true),
		"create the schema at startup; IF NOT EXISTS only creates what is absent")
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
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if f.showVer {
		fmt.Println("fleet-apiserver", version)
		return nil
	}

	log := newLogger(f.logLevel)
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
	if db != nil {
		policySource = sqlstore.NewPolicySource(db, ratelimit.Policy{})
		keyStore = sqlstore.NewKeyStore(db, sqlstore.NewTTLCache(5*time.Minute, 4096))
		quotaStore = sqlstore.NewQuota(db)
	}

	srv, err := apiserver.NewServer(apiserver.Config{
		Listen:          f.listen,
		Blobs:           store,
		Hub:             h,
		PullConcurrency: f.pullConc,
		FileConcurrency: f.fileConc,
		AllowedOrigins:  f.allowedOrigins,
		Version:         version,
		DB:              db,
		Policies:        policySource,
		Keys:            keyStore,
		Quota:           quotaStore,
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

// openStore picks the object backend.
//
// S3 wins when an endpoint is configured; otherwise a directory under --data.
// The fallback is deliberate: an operator evaluating Fleet should be able to
// pull a small model and see the whole path work before standing up MinIO, and
// a single-node install is a legitimate deployment, not a mistake.
func openStore() (blobstore.Store, error) {
	if os.Getenv("FLEET_S3_ENDPOINT") != "" {
		store, err := blobstore.S3FromEnv(context.Background())
		if err != nil {
			return nil, fmt.Errorf("configuring S3: %w", err)
		}
		return store, nil
	}
	dir := envOr("FLEET_DATA_DIR", "./fleet-data")
	abs, err := filepath.Abs(filepath.Join(dir, "weights"))
	if err != nil {
		return nil, err
	}
	return blobstore.NewFS(abs)
}

func pickHub(f flags) (hub.Hub, string) {
	if f.dev {
		stub := hub.NewStub()
		if f.devDelay > 0 {
			stub.PerFileDelay = f.devDelay
		}
		return stub, "synthetic (--dev)"
	}
	if f.hubToken != "" {
		return hub.NewHTTP(f.hubToken, f.fileConc), f.hubURL
	}
	return hub.NewHTTP("", f.fileConc), f.hubURL
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

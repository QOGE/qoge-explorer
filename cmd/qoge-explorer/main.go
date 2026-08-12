// Command qoge-explorer is the entry point for the QOGE block explorer
// service. It implements RPC connectivity verification, schema migrations,
// historical/live chain indexing (`index`), and a read-only JSON API plus —
// as of Phase 2E.1 — a server-rendered HTML explorer UI, both served by
// `serve` over the already-indexed PostgreSQL database. `index` and `serve`
// remain deliberately separate processes; internal/api and internal/web are
// sibling presentation layers over the same internal/query.Store, composed
// under one HTTP server, never calling one another.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/QOGE/qoge-explorer/internal/api"
	"github.com/QOGE/qoge-explorer/internal/config"
	"github.com/QOGE/qoge-explorer/internal/decode"
	"github.com/QOGE/qoge-explorer/internal/deployments"
	"github.com/QOGE/qoge-explorer/internal/indexer"
	"github.com/QOGE/qoge-explorer/internal/logging"
	"github.com/QOGE/qoge-explorer/internal/mempool"
	"github.com/QOGE/qoge-explorer/internal/query"
	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/store"
	"github.com/QOGE/qoge-explorer/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	log := logging.New(cfg.LogLevel, cfg.LogJSON)

	switch os.Args[1] {
	case "check-rpc":
		os.Exit(runCheckRPC(cfg, log))
	case "serve":
		os.Exit(runServe(cfg, log))
	case "migrate":
		os.Exit(runMigrate(cfg, log, os.Args[2:]))
	case "index":
		os.Exit(runIndex(cfg, log))
	case "backfill-accounting":
		os.Exit(runBackfillAccounting(cfg, log))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `qoge-explorer - QOGE block explorer (Phase 2E.1: read-only query layer + JSON API + HTML explorer UI)

Usage:
  qoge-explorer check-rpc          Connect to the configured Qogecoin Core node
                                    and print non-secret status information.
  qoge-explorer serve               Start the read-only JSON API and HTML
                                     explorer UI over the already-indexed
                                     PostgreSQL database (loopback by
                                     default). Does not index; does not
                                     require Core RPC credentials.
  qoge-explorer migrate up          Apply every not-yet-applied migration.
  qoge-explorer migrate down [n]    Roll back the n most recent migrations
                                     (default 1).
  qoge-explorer migrate version     Print the current schema version.
  qoge-explorer index               Validate Core/network/database safety,
                                     then historically sync from genesis (or
                                     resume from the last checkpoint) and
                                     poll for new blocks/reorgs until
                                     SIGINT/SIGTERM. Also runs the mempool
                                     cache synchronizer (Phase 2F.1) against
                                     the same Core node, isolated from
                                     confirmed chain state; a mempool
                                     refresh failure is logged and retried,
                                     never halts confirmed indexing. No
                                     API/UI. Run this as a separate process
                                     from serve.
  qoge-explorer backfill-accounting Reconstruct block_accounting rows
                                     (Phase 2H.1) for every block already in
                                     the database — canonical and orphaned
                                     alike — from already-indexed PostgreSQL
                                     data only; never calls Core RPC. Only
                                     needed on a database that was indexed
                                     before migration 0004 existed; a
                                     database indexed entirely on or after
                                     0004 already has every row (ApplyBlock
                                     writes it live). Idempotent and safely
                                     restartable — rerun after an
                                     interruption to continue. REQUIRES the
                                     index process to be stopped first: this
                                     command takes a PostgreSQL
                                     advisory lock against a second
                                     CONCURRENT backfill-accounting run, but
                                     that lock does not, and cannot,
                                     serialize against a live indexer
                                     writing new blocks at the same time —
                                     see docs/ARCHITECTURE.md §26.

Configuration is read from environment variables:
  QOGE_RPC_HOST             default 127.0.0.1 (check-rpc, index)
  QOGE_RPC_PORT             default 8332      (check-rpc, index)
  QOGE_RPC_USER             required for check-rpc, index
  QOGE_RPC_PASSWORD         required for check-rpc, index
  QOGE_RPC_TLS              default false     (check-rpc, index)
  QOGE_RPC_TIMEOUT_SECONDS  default 30        (check-rpc, index)
  QOGE_DATABASE_URL         required for migrate/index/serve/backfill-accounting,
                             e.g. postgres://user:pass@host:5432/dbname
  QOGE_MIGRATIONS_DIR       default ./migrations (migrate)
  QOGE_NETWORK              required for index; must exactly match Core's
                             getblockchaininfo "chain" (e.g. main/test/regtest)
  QOGE_INDEX_POLL_SECONDS   default 10 (index; live-loop wait once caught up)
  QOGE_HTTP_ADDR            default 127.0.0.1:8532 (serve)
  QOGE_LOG_LEVEL            default info
  QOGE_LOG_JSON             default false`)
}

func runCheckRPC(cfg config.Config, log interface {
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}) int {
	if err := cfg.RPC.Validate(); err != nil {
		log.Error("config error", "error", err)
		return 1
	}
	log.Info("connecting to qogecoin core", "rpc", cfg.RPC.Redacted())

	client := rpc.New(rpc.Config{
		Host:     cfg.RPC.Host,
		Port:     cfg.RPC.Port,
		User:     cfg.RPC.User,
		Password: cfg.RPC.Password,
		UseTLS:   cfg.RPC.UseTLS,
		Timeout:  time.Duration(cfg.RPC.Timeout) * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.RPC.Timeout)*time.Second)
	defer cancel()

	netInfo, err := client.GetNetworkInfo(ctx)
	if err != nil {
		log.Error("getnetworkinfo failed", "error", err)
		return 1
	}

	chainInfo, err := client.GetBlockchainInfo(ctx)
	if err != nil {
		log.Error("getblockchaininfo failed", "error", err)
		return 1
	}

	idxInfo, err := client.GetIndexInfo(ctx)
	if err != nil {
		log.Error("getindexinfo failed", "error", err)
		return 1
	}

	fmt.Println("QOGE Core RPC connectivity check")
	fmt.Println("---------------------------------")
	fmt.Printf("network:            %s\n", chainInfo.Chain)
	fmt.Printf("core version:       %s (protocol %d)\n", netInfo.SubVersion, netInfo.ProtocolVersion)
	fmt.Printf("connections:        %d\n", netInfo.Connections)
	fmt.Printf("block height:       %d\n", chainInfo.Blocks)
	fmt.Printf("header height:      %d\n", chainInfo.Headers)
	fmt.Printf("best block hash:    %s\n", chainInfo.BestBlockHash)
	fmt.Printf("verification prog:  %.6f\n", chainInfo.VerificationProgress)
	fmt.Printf("initial block dl:   %t\n", chainInfo.InitialBlockDownload)
	fmt.Printf("pruned:             %t\n", chainInfo.Pruned)
	if idxInfo.TxIndex != nil {
		fmt.Printf("txindex synced:     %t (height %d)\n", idxInfo.TxIndex.Synced, idxInfo.TxIndex.BestBlockHeight)
	} else {
		fmt.Printf("txindex synced:     unavailable (txindex may be disabled)\n")
	}

	log.Info("rpc connectivity check succeeded")
	return 0
}

// newRootHandler composes internal/api and internal/web as sibling
// handlers over the SAME query.Store: /api/, /healthz, and /readyz go to
// the JSON API exactly as before Phase 2E.1; everything else goes to the
// HTML explorer. Neither handler calls the other over HTTP — see
// runServe's doc comment. Extracted from runServe so the composition
// itself (which route goes where, and that each side responds in its own
// content type) can be exercised directly in tests without starting a
// real listener — see serve_test.go.
func newRootHandler(q *query.Store, log *slog.Logger) http.Handler {
	apiHandler := api.New(q, log)
	webHandler := web.New(q, log)

	rootMux := http.NewServeMux()
	rootMux.Handle("/api/", apiHandler)
	rootMux.Handle("/healthz", apiHandler)
	rootMux.Handle("/readyz", apiHandler)
	rootMux.Handle("/", webHandler)
	return rootMux
}

// runServe starts the Phase 2E.1 read-only JSON API and HTML explorer UI
// over the already-indexed PostgreSQL database. It never starts indexing
// and never requires Core RPC credentials — see cmd/qoge-explorer's package
// doc comment and docs/ARCHITECTURE.md §19 "index and serve remain
// separate". internal/api and internal/web are wired as sibling handlers
// over the SAME query.Store — never composed via a loopback HTTP call from
// one into the other — and are delegated to by path prefix from one root
// mux, so JSON and HTML routes are served by the one HTTP process/listener
// this command has always started, per docs/ARCHITECTURE.md §20.
func runServe(cfg config.Config, log *slog.Logger) int {
	if cfg.DatabaseURL == "" {
		log.Error("config error", "error", "QOGE_DATABASE_URL is not set")
		return 1
	}

	connectCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	pool, err := store.Connect(connectCtx, cfg.DatabaseURL)
	cancel()
	if err != nil {
		log.Error("database connection failed", "error", err)
		return 1
	}
	defer pool.Close()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           newRootHandler(query.New(pool), log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("starting read-only API + web server (index and serve remain separate processes)", "addr", cfg.HTTPAddr)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			log.Error("http server failed", "error", err)
			return 1
		}
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("http server shutdown failed", "error", err)
			return 1
		}
	}
	log.Info("http server stopped")
	return 0
}

func runMigrate(cfg config.Config, log interface {
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}, args []string) int {
	if cfg.DatabaseURL == "" {
		log.Error("config error", "error", "QOGE_DATABASE_URL is not set")
		return 1
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qoge-explorer migrate {up|down [n]|version}")
		return 2
	}

	migrationsDir := os.Getenv("QOGE_MIGRATIONS_DIR")
	if migrationsDir == "" {
		migrationsDir = "migrations"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database connection failed", "error", err)
		return 1
	}
	defer pool.Close()

	migrations, err := store.LoadMigrations(os.DirFS(migrationsDir))
	if err != nil {
		log.Error("loading migrations failed", "dir", migrationsDir, "error", err)
		return 1
	}

	switch args[0] {
	case "up":
		applied, err := store.Up(ctx, pool, migrations)
		if err != nil {
			log.Error("migrate up failed", "error", err)
			return 1
		}
		if len(applied) == 0 {
			fmt.Println("already up to date")
		} else {
			fmt.Printf("applied %d migration(s): %v\n", len(applied), applied)
		}
		return 0

	case "down":
		steps := 1
		if len(args) > 1 {
			n, err := strconv.Atoi(args[1])
			if err != nil || n <= 0 {
				fmt.Fprintf(os.Stderr, "invalid step count %q: must be a positive integer\n", args[1])
				return 2
			}
			steps = n
		}
		rolledBack, err := store.Down(ctx, pool, migrations, steps)
		if err != nil {
			log.Error("migrate down failed", "error", err)
			return 1
		}
		if len(rolledBack) == 0 {
			fmt.Println("nothing to roll back")
		} else {
			fmt.Printf("rolled back %d migration(s): %v\n", len(rolledBack), rolledBack)
		}
		return 0

	case "version":
		version, err := store.CurrentVersion(ctx, pool)
		if err != nil {
			log.Error("reading schema version failed", "error", err)
			return 1
		}
		fmt.Printf("schema version: %d\n", version)
		return 0

	default:
		fmt.Fprintf(os.Stderr, "unknown migrate subcommand: %s\n", args[0])
		return 2
	}
}

// runBackfillAccounting reconstructs block_accounting (Phase 2H.1) for
// every already-indexed block from PostgreSQL data alone — see
// store.Store.BackfillAccounting's doc comment for the full idempotency/
// concurrency contract. It does not connect to Core RPC and does not
// require any RPC configuration.
func runBackfillAccounting(cfg config.Config, log *slog.Logger) int {
	if cfg.DatabaseURL == "" {
		log.Error("config error", "error", "QOGE_DATABASE_URL is not set")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database connection failed", "error", err)
		return 1
	}
	defer pool.Close()

	st := store.New(pool)

	before, err := store.CheckAccountingCompleteness(ctx, pool)
	if err != nil {
		log.Error("pre-backfill completeness check failed", "error", err)
		return 1
	}
	log.Info("starting accounting backfill",
		"blocks", before.BlockCount, "existing_accounting_rows", before.AccountingCount, "missing", before.MissingCount)

	start := time.Now()
	result, err := st.BackfillAccounting(ctx)
	elapsed := time.Since(start)
	if err != nil {
		log.Error("accounting backfill failed", "error", err, "elapsed", elapsed.String())
		return 1
	}

	after, err := store.CheckAccountingCompleteness(ctx, pool)
	if err != nil {
		log.Error("post-backfill completeness check failed", "error", err)
		return 1
	}

	fmt.Println("qoge-explorer backfill-accounting")
	fmt.Println("----------------------------------")
	fmt.Printf("blocks considered:   %d\n", result.TotalBlocks)
	fmt.Printf("rows inserted:       %d\n", result.Inserted)
	fmt.Printf("rows verified:       %d\n", result.Verified)
	fmt.Printf("blocks (final):      %d\n", after.BlockCount)
	fmt.Printf("accounting (final):  %d\n", after.AccountingCount)
	fmt.Printf("missing (final):     %d\n", after.MissingCount)
	fmt.Printf("elapsed:             %s\n", elapsed)

	if after.MissingCount != 0 {
		log.Error("accounting backfill completed but coverage is still incomplete", "missing", after.MissingCount)
		return 1
	}

	log.Info("accounting backfill complete", "elapsed", elapsed.String())
	return 0
}

// runIndex validates the Core/database/network environment, then runs
// historical sync + the live reorg-aware polling loop
// (docs/ARCHITECTURE.md §18) until SIGINT/SIGTERM. It never starts
// indexing against a pruned node, a node in initial block download, or a
// node whose network doesn't exactly match QOGE_NETWORK — see
// indexer.ValidateStartup.
func runIndex(cfg config.Config, log *slog.Logger) int {
	if err := cfg.RPC.Validate(); err != nil {
		log.Error("config error", "error", err)
		return 1
	}
	if cfg.DatabaseURL == "" {
		log.Error("config error", "error", "QOGE_DATABASE_URL is not set")
		return 1
	}
	if cfg.Network == "" {
		log.Error("config error", "error", "QOGE_NETWORK is not set")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := rpc.New(rpc.Config{
		Host:     cfg.RPC.Host,
		Port:     cfg.RPC.Port,
		User:     cfg.RPC.User,
		Password: cfg.RPC.Password,
		UseTLS:   cfg.RPC.UseTLS,
		Timeout:  time.Duration(cfg.RPC.Timeout) * time.Second,
	})

	log.Info("connecting to qogecoin core", "rpc", cfg.RPC.Redacted(), "network", cfg.Network)

	startupCtx, cancelStartup := context.WithTimeout(ctx, time.Duration(cfg.RPC.Timeout)*time.Second)
	chainInfo, err := client.GetBlockchainInfo(startupCtx)
	cancelStartup()
	if err != nil {
		log.Error("getblockchaininfo failed", "error", err)
		return 1
	}
	if err := indexer.ValidateStartup(chainInfo, cfg.Network); err != nil {
		log.Error("startup safety check failed", "error", err)
		return 1
	}
	log.Info("startup safety checks passed",
		"chain", chainInfo.Chain, "blocks", chainInfo.Blocks, "pruned", chainInfo.Pruned)

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database connection failed", "error", err)
		return 1
	}
	defer pool.Close()

	resolver := decode.NewCoreAddressResolver(client)
	st := store.New(pool)
	pollInterval := time.Duration(cfg.IndexPollSeconds) * time.Second
	idx := indexer.New(client, st, resolver, pollInterval, log)

	// The mempool synchronizer runs alongside confirmed indexing, on the
	// same process (it needs the same Core RPC credentials `serve`
	// deliberately never has — docs/ARCHITECTURE.md §22) but against
	// entirely separate PostgreSQL tables (migrations/
	// 0002_mempool_cache.up.sql), sharing only the read-only confirmed
	// checkpoint and the same address resolver (safe to share: it's just
	// an in-memory pubkey->address cache guarded by its own mutex — see
	// decode.CoreAddressResolver). mempoolCtx is cancelled explicitly
	// after idx.Run returns, whether or not idx.Run itself observed ctx
	// cancellation, so a confirmed-indexing halt (a real error, not a
	// clean shutdown) still stops the mempool synchronizer rather than
	// leaking its goroutine.
	mempoolCtx, cancelMempool := context.WithCancel(ctx)
	defer cancelMempool()

	mempoolStore := mempool.NewStore(pool)
	mempoolSync := mempool.New(client, st, mempoolStore, resolver, mempool.DefaultPollInterval, log)

	var mempoolWG sync.WaitGroup
	mempoolWG.Add(1)
	go func() {
		defer mempoolWG.Done()
		mempoolSync.Run(mempoolCtx)
	}()

	// The deployment observer (Phase 2G.1) runs alongside confirmed
	// indexing and the mempool synchronizer, on the same process (it
	// needs the same Core RPC credentials `serve` deliberately never
	// has) but against entirely separate PostgreSQL tables
	// (chain_deployments, deployment_state — migrations/
	// 0003_deployment_state.up.sql), sharing only the read-only confirmed
	// checkpoint. deploymentCtx is cancelled explicitly after idx.Run
	// returns, exactly like mempoolCtx, so a confirmed-indexing halt
	// stops the deployment observer rather than leaking its goroutine.
	deploymentCtx, cancelDeployment := context.WithCancel(ctx)
	defer cancelDeployment()

	deploymentStore := deployments.NewStore(pool)
	deploymentSync := deployments.New(client, st, deploymentStore, deployments.DefaultPollInterval, log)

	var deploymentWG sync.WaitGroup
	deploymentWG.Add(1)
	go func() {
		defer deploymentWG.Done()
		deploymentSync.Run(deploymentCtx)
	}()

	log.Info("starting indexer", "poll_interval", pollInterval.String())
	// idx.Run returns nil on a clean SIGINT/SIGTERM shutdown; any non-nil
	// error is a deterministic halt (decode/store/integrity/deep-reorg
	// failure) that a process supervisor should treat as a failed exit,
	// not silently retry. A mempool refresh failure NEVER surfaces here —
	// mempool.Synchronizer.Run logs and retries its own failures
	// internally and never returns an error at all (docs/ARCHITECTURE.md
	// §22): confirmed indexing remains authoritative and is never halted
	// by mempool trouble. A deployment observation failure never
	// surfaces here either, for the identical reason
	// (deployments.Synchronizer.Run — docs/ARCHITECTURE.md §24).
	runErr := idx.Run(ctx)

	cancelMempool()
	mempoolWG.Wait()
	cancelDeployment()
	deploymentWG.Wait()

	if runErr != nil {
		log.Error("indexer halted", "error", runErr)
		return 1
	}
	log.Info("indexer stopped")
	return 0
}

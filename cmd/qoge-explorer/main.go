// Command qoge-explorer is the entry point for the QOGE block explorer
// service. Phase 1 only implements RPC connectivity verification and a
// loopback health endpoint; no chain indexing happens yet.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/QOGE/qoge-explorer/internal/config"
	"github.com/QOGE/qoge-explorer/internal/logging"
	"github.com/QOGE/qoge-explorer/internal/rpc"
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
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `qoge-explorer - QOGE block explorer (Phase 1: reconnaissance only)

Usage:
  qoge-explorer check-rpc   Connect to the configured Qogecoin Core node and
                            print non-secret status information.
  qoge-explorer serve       Start the loopback-only health endpoint.

Configuration is read from environment variables:
  QOGE_RPC_HOST             default 127.0.0.1
  QOGE_RPC_PORT             default 8332
  QOGE_RPC_USER             required
  QOGE_RPC_PASSWORD         required
  QOGE_RPC_TLS              default false
  QOGE_RPC_TIMEOUT_SECONDS  default 30
  QOGE_HTTP_ADDR            default 127.0.0.1:8532
  QOGE_LOG_LEVEL            default info
  QOGE_LOG_JSON             default false`)
}

func runCheckRPC(cfg config.Config, log interface {
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}) int {
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

func runServe(cfg config.Config, log interface {
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}) int {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","phase":"1-recon-only","indexing":false}`))
	})

	log.Info("starting loopback health server (no indexing, no public exposure)", "addr", cfg.HTTPAddr)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("http server failed", "error", err)
		return 1
	}
	return 0
}

// Package config loads runtime configuration for the QOGE explorer.
//
// Configuration is sourced from environment variables only for Phase 1.
// RPC credentials are never logged or printed; use Config.Redacted() for
// any diagnostic output.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds all runtime configuration for the explorer service.
type Config struct {
	RPC      RPCConfig
	HTTPAddr string // bind address for the internal health/API server (loopback only in Phase 1)
	LogLevel string // debug, info, warn, error
	LogJSON  bool
}

// RPCConfig holds Qogecoin Core JSON-RPC connection parameters.
type RPCConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	UseTLS   bool
	Timeout  int // seconds
}

// Load builds a Config from environment variables, applying QOGE explorer
// defaults that match the local reference node described in project docs
// (localhost:8332). It does not read qogecoin.conf directly, so the two
// processes never need to share file permissions.
func Load() (Config, error) {
	cfg := Config{
		RPC: RPCConfig{
			Host:     getEnv("QOGE_RPC_HOST", "127.0.0.1"),
			Port:     getEnvInt("QOGE_RPC_PORT", 8332),
			User:     getEnv("QOGE_RPC_USER", ""),
			Password: getEnv("QOGE_RPC_PASSWORD", ""),
			UseTLS:   getEnvBool("QOGE_RPC_TLS", false),
			Timeout:  getEnvInt("QOGE_RPC_TIMEOUT_SECONDS", 30),
		},
		HTTPAddr: getEnv("QOGE_HTTP_ADDR", "127.0.0.1:8532"),
		LogLevel: getEnv("QOGE_LOG_LEVEL", "info"),
		LogJSON:  getEnvBool("QOGE_LOG_JSON", false),
	}

	if cfg.RPC.User == "" {
		return cfg, fmt.Errorf("QOGE_RPC_USER is not set")
	}
	if cfg.RPC.Password == "" {
		return cfg, fmt.Errorf("QOGE_RPC_PASSWORD is not set")
	}

	return cfg, nil
}

// Redacted returns a copy of the RPC config safe to log: the password is
// replaced with a fixed-length placeholder regardless of its real length.
func (r RPCConfig) Redacted() string {
	passState := "unset"
	if r.Password != "" {
		passState = "set"
	}
	return fmt.Sprintf("host=%s port=%d user=%s password=%s tls=%t timeout=%ds",
		r.Host, r.Port, r.User, passState, r.UseTLS, r.Timeout)
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

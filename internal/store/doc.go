// Package store implements PostgreSQL persistence: one SQL transaction per
// indexed block, idempotent writes, and set-based (not additive) address
// balance maintenance. Reserved for Phase 2 — see docs/ARCHITECTURE.md §3, §4.
package store

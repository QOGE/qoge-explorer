package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration is one versioned schema change, loaded from a pair of
// NNNN_name.up.sql / NNNN_name.down.sql files.
type Migration struct {
	Version int64
	Name    string
	UpSQL   string
	DownSQL string
}

var migrationFileRe = regexp.MustCompile(`^(\d+)_(.+)\.(up|down)\.sql$`)

// LoadMigrations reads every NNNN_name.up.sql/.down.sql pair directly in
// fsys (non-recursive), and returns them sorted by version ascending.
//
// A migration missing its down file is rejected: Phase 2B.1 requires every
// migration to be reversible (task item 10/11 — "migrations roll back
// cleanly"), so a one-way migration is a authoring mistake, not a valid
// state, and LoadMigrations fails loudly rather than silently allowing
// Down() to later error on a partial rollback.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations dir: %w", err)
	}

	byVersion := map[int64]*Migration{}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		m := migrationFileRe.FindStringSubmatch(entry.Name())
		if m == nil {
			continue // ignore non-migration files (README, etc.)
		}
		version, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("store: migration %s: bad version: %w", entry.Name(), err)
		}
		name, direction := m[2], m[3]

		content, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", entry.Name(), err)
		}

		mig, ok := byVersion[version]
		if !ok {
			mig = &Migration{Version: version, Name: name}
			byVersion[version] = mig
		} else if mig.Name != name {
			return nil, fmt.Errorf("store: migration version %d has inconsistent names %q and %q", version, mig.Name, name)
		}

		switch direction {
		case "up":
			mig.UpSQL = string(content)
		case "down":
			mig.DownSQL = string(content)
		}
	}

	migrations := make([]Migration, 0, len(byVersion))
	for _, mig := range byVersion {
		if mig.UpSQL == "" {
			return nil, fmt.Errorf("store: migration %04d_%s missing .up.sql", mig.Version, mig.Name)
		}
		if mig.DownSQL == "" {
			return nil, fmt.Errorf("store: migration %04d_%s missing .down.sql (every migration must be reversible)", mig.Version, mig.Name)
		}
		migrations = append(migrations, *mig)
	}
	if len(migrations) == 0 {
		return nil, ErrNoMigrations
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })

	return migrations, nil
}

const ensureSchemaMigrationsSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    BIGINT PRIMARY KEY,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// checksum returns the hex-encoded SHA-256 of a migration's UpSQL, used to
// detect a migration file being edited after it was already applied (task
// item 5: "checksum drift detection"). Only UpSQL is checksummed — DownSQL
// changing after the fact doesn't affect what's already been applied to the
// database, only what a future rollback would run, so it isn't part of the
// "does the applied schema still match this file" question this guards.
func checksum(upSQL string) string {
	sum := sha256.Sum256([]byte(upSQL))
	return hex.EncodeToString(sum[:])
}

// ensureSchemaMigrationsTable creates the migration-tracking table if it
// doesn't already exist. It is itself not a tracked migration — it must
// exist before any tracked migration can be recorded.
func ensureSchemaMigrationsTable(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, ensureSchemaMigrationsSQL)
	if err != nil {
		return fmt.Errorf("store: ensure schema_migrations: %w", err)
	}
	return nil
}

// AppliedVersions returns the set of migration versions currently recorded
// as applied.
func AppliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[int64]bool, error) {
	records, err := appliedRecords(ctx, pool)
	if err != nil {
		return nil, err
	}
	applied := make(map[int64]bool, len(records))
	for v := range records {
		applied[v] = true
	}
	return applied, nil
}

// appliedRecord is what schema_migrations actually recorded for one applied
// migration.
type appliedRecord struct {
	name     string
	checksum string
}

// appliedRecords returns every row currently in schema_migrations, keyed by
// version.
func appliedRecords(ctx context.Context, pool *pgxpool.Pool) (map[int64]appliedRecord, error) {
	if err := ensureSchemaMigrationsTable(ctx, pool); err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, "SELECT version, name, checksum FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("store: query applied migrations: %w", err)
	}
	defer rows.Close()

	records := map[int64]appliedRecord{}
	for rows.Next() {
		var v int64
		var rec appliedRecord
		if err := rows.Scan(&v, &rec.name, &rec.checksum); err != nil {
			return nil, fmt.Errorf("store: scan applied migration record: %w", err)
		}
		records[v] = rec
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate applied migrations: %w", err)
	}
	return records, nil
}

// VerifyChecksums compares every already-applied migration for which a
// local definition exists (in migrations) against its recorded name and
// UpSQL checksum. It returns an error describing the exact version/name if
// either has changed since the migration was applied — i.e. if
// NNNN_name.up.sql was edited (or renamed) after being applied to this
// database. Migrations that exist locally but haven't been applied yet, or
// that were applied by a version of the migrations directory that no
// longer has a local file for them, are not an error here (LoadMigrations
// already refuses to load a migration missing its .down.sql, which is the
// only case that would silently orphan an applied version).
func VerifyChecksums(ctx context.Context, pool *pgxpool.Pool, migrations []Migration) error {
	applied, err := appliedRecords(ctx, pool)
	if err != nil {
		return err
	}

	for _, mig := range migrations {
		rec, ok := applied[mig.Version]
		if !ok {
			continue // not yet applied; nothing recorded to drift from
		}
		if rec.name != mig.Name {
			return fmt.Errorf("store: migration %d was applied as %q but is now loaded as %q — a migration's name must not change after it has been applied", mig.Version, rec.name, mig.Name)
		}
		want := checksum(mig.UpSQL)
		if rec.checksum != want {
			return fmt.Errorf("store: migration %04d_%s was modified after being applied (recorded checksum %s, current file checksum %s) — the applied database schema may no longer match this migration file; do not edit an applied migration, add a new one instead", mig.Version, mig.Name, rec.checksum, want)
		}
	}
	return nil
}

// CurrentVersion returns the highest applied migration version, or 0 if
// none have been applied yet.
func CurrentVersion(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	applied, err := AppliedVersions(ctx, pool)
	if err != nil {
		return 0, err
	}
	var max int64
	for v := range applied {
		if v > max {
			max = v
		}
	}
	return max, nil
}

// Up applies every migration in migrations whose version is not yet
// recorded as applied, in ascending version order. Each migration's DDL
// and its schema_migrations bookkeeping row are committed in a single
// transaction — a failure partway through a migration leaves the database
// exactly as it was before that migration started (Postgres DDL is
// transactional), and never records a migration as applied unless it fully
// succeeded. Returns the versions actually applied, in order.
func Up(ctx context.Context, pool *pgxpool.Pool, migrations []Migration) ([]int64, error) {
	if err := ensureSchemaMigrationsTable(ctx, pool); err != nil {
		return nil, err
	}
	// Fail loudly, before touching anything, if any already-applied
	// migration's local .sql file has been edited since it was applied.
	if err := VerifyChecksums(ctx, pool, migrations); err != nil {
		return nil, err
	}
	applied, err := AppliedVersions(ctx, pool)
	if err != nil {
		return nil, err
	}

	var didApply []int64
	for _, mig := range migrations {
		if applied[mig.Version] {
			continue
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			return didApply, fmt.Errorf("store: begin migration %04d_%s: %w", mig.Version, mig.Name, err)
		}

		if _, err := tx.Exec(ctx, mig.UpSQL); err != nil {
			_ = tx.Rollback(ctx)
			return didApply, fmt.Errorf("store: apply migration %04d_%s: %w", mig.Version, mig.Name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)",
			mig.Version, mig.Name, checksum(mig.UpSQL)); err != nil {
			_ = tx.Rollback(ctx)
			return didApply, fmt.Errorf("store: record migration %04d_%s: %w", mig.Version, mig.Name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return didApply, fmt.Errorf("store: commit migration %04d_%s: %w", mig.Version, mig.Name, err)
		}

		didApply = append(didApply, mig.Version)
	}
	return didApply, nil
}

// Down rolls back the `steps` most-recently-applied migrations, in
// descending version order, using each migration's DownSQL. Like Up, each
// rollback's DDL and its schema_migrations deletion are one transaction.
// Returns the versions actually rolled back, in the order they were rolled
// back (most recent first).
func Down(ctx context.Context, pool *pgxpool.Pool, migrations []Migration, steps int) ([]int64, error) {
	if steps <= 0 {
		return nil, fmt.Errorf("store: Down: steps must be positive, got %d", steps)
	}
	if err := ensureSchemaMigrationsTable(ctx, pool); err != nil {
		return nil, err
	}
	// Same pre-flight check as Up: refuse to roll back anything if an
	// already-applied migration's local file has drifted from what was
	// actually applied — its DownSQL may no longer be the correct inverse
	// of what's really in the database.
	if err := VerifyChecksums(ctx, pool, migrations); err != nil {
		return nil, err
	}
	applied, err := AppliedVersions(ctx, pool)
	if err != nil {
		return nil, err
	}

	byVersion := make(map[int64]Migration, len(migrations))
	for _, mig := range migrations {
		byVersion[mig.Version] = mig
	}

	// Applied versions, descending, so we roll back the newest first.
	var appliedVersions []int64
	for v := range applied {
		appliedVersions = append(appliedVersions, v)
	}
	sort.Slice(appliedVersions, func(i, j int) bool { return appliedVersions[i] > appliedVersions[j] })

	var didRollback []int64
	for i := 0; i < steps && i < len(appliedVersions); i++ {
		version := appliedVersions[i]
		mig, ok := byVersion[version]
		if !ok {
			return didRollback, fmt.Errorf("store: applied migration version %d has no loaded definition (missing .sql files?)", version)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			return didRollback, fmt.Errorf("store: begin rollback of migration %04d_%s: %w", mig.Version, mig.Name, err)
		}

		if _, err := tx.Exec(ctx, mig.DownSQL); err != nil {
			_ = tx.Rollback(ctx)
			return didRollback, fmt.Errorf("store: roll back migration %04d_%s: %w", mig.Version, mig.Name, err)
		}
		if _, err := tx.Exec(ctx, "DELETE FROM schema_migrations WHERE version = $1", mig.Version); err != nil {
			_ = tx.Rollback(ctx)
			return didRollback, fmt.Errorf("store: unrecord migration %04d_%s: %w", mig.Version, mig.Name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return didRollback, fmt.Errorf("store: commit rollback of migration %04d_%s: %w", mig.Version, mig.Name, err)
		}

		didRollback = append(didRollback, mig.Version)
	}
	return didRollback, nil
}

// ErrNoMigrations is returned by LoadMigrations callers when a migrations
// directory exists but contains no recognizable migration files.
var ErrNoMigrations = errors.New("store: no migration files found")

// Package store handles idempotency key lookups.
//
// Two modes, toggled by IDEMPOTENCY_CACHE env var:
//
//   redis   (default) — check Redis first; fall back to Postgres on a miss.
//             A cache hit avoids a Postgres round-trip entirely.
//   postgres           — skip Redis; every check hits Postgres directly.
//
// Run scripts/benchmark-idempotency.sh to compare p95 latency under a
// retry-heavy workload. The difference is the Postgres round-trip cost
// that Redis removes from the hot path on duplicate/retry requests.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

type IdempotencyStore struct {
	db    *sql.DB
	rdb   *redis.Client
	mode  string // "redis" | "postgres"
}

func NewIdempotencyStore(db *sql.DB, rdb *redis.Client) *IdempotencyStore {
	mode := os.Getenv("IDEMPOTENCY_CACHE")
	if mode != "postgres" {
		mode = "redis"
	}
	return &IdempotencyStore{db: db, rdb: rdb, mode: mode}
}

// Get returns the stored result for a key, or ("", false) on a miss.
func (s *IdempotencyStore) Get(ctx context.Context, key string) (string, bool) {
	if s.mode == "redis" {
		val, err := s.rdb.Get(ctx, "idem:"+key).Result()
		if err == nil {
			return val, true
		}
		// cache miss — fall through to Postgres
	}

	// Postgres source of truth
	var result string
	err := s.db.QueryRowContext(ctx,
		`SELECT result FROM idempotency_keys WHERE key=$1`, key,
	).Scan(&result)
	if err != nil {
		return "", false
	}

	// Backfill Redis on a Postgres hit (cache was empty or mode=postgres)
	if s.mode == "redis" {
		s.rdb.Set(ctx, "idem:"+key, result, 24*time.Hour)
	}
	return result, true
}

// Set stores the result for a key in both Postgres and Redis.
func (s *IdempotencyStore) Set(ctx context.Context, key string, value interface{}) error {
	b, _ := json.Marshal(value)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO idempotency_keys (key, result) VALUES ($1,$2)
		 ON CONFLICT (key) DO NOTHING`, key, string(b),
	)
	if err != nil {
		return err
	}
	if s.mode == "redis" {
		s.rdb.Set(ctx, "idem:"+key, string(b), 24*time.Hour)
	}
	return nil
}

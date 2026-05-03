package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	_ "github.com/lib/pq"
)

var (
	pool    *sql.DB
	poolErr error
	once    sync.Once
)

// Connect returns the shared database connection pool, initialising it on first call.
func Connect() (*sql.DB, error) {
	once.Do(func() {
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			poolErr = fmt.Errorf("DATABASE_URL environment variable is not set")
			return
		}

		var err error
		pool, err = sql.Open("postgres", dsn)
		if err != nil {
			poolErr = fmt.Errorf("sql.Open: %w", err)
			return
		}

		pool.SetMaxOpenConns(15)
		pool.SetMaxIdleConns(5)
		pool.SetConnMaxLifetime(5 * time.Minute)
		pool.SetConnMaxIdleTime(2 * time.Minute)

		if err = pool.Ping(); err != nil {
			poolErr = fmt.Errorf("db ping failed: %w", err)
		}
	})
	return pool, poolErr
}

// Migrate runs idempotent DDL statements to create or update the schema.
func Migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE EXTENSION IF NOT EXISTS "pgcrypto"`,

		`CREATE TABLE IF NOT EXISTS profiles (
			id                  TEXT PRIMARY KEY,
			name                TEXT UNIQUE NOT NULL,
			gender              TEXT        NOT NULL DEFAULT '',
			gender_probability  FLOAT       NOT NULL DEFAULT 0,
			sample_size         INT         NOT NULL DEFAULT 0,
			age                 INT         NOT NULL DEFAULT 0,
			age_group           TEXT        NOT NULL DEFAULT '',
			country_id          TEXT        NOT NULL DEFAULT '',
			country_name        TEXT        NOT NULL DEFAULT '',
			country_probability FLOAT       NOT NULL DEFAULT 0,
			created_at          TEXT        NOT NULL
		)`,

		// Add columns that may be missing from a Stage 2 DB.
		`ALTER TABLE profiles ADD COLUMN IF NOT EXISTS gender_probability  FLOAT NOT NULL DEFAULT 0`,
		`ALTER TABLE profiles ADD COLUMN IF NOT EXISTS sample_size         INT   NOT NULL DEFAULT 0`,
		`ALTER TABLE profiles ADD COLUMN IF NOT EXISTS country_name        TEXT  NOT NULL DEFAULT ''`,
		`ALTER TABLE profiles ADD COLUMN IF NOT EXISTS country_probability FLOAT NOT NULL DEFAULT 0`,

		`CREATE TABLE IF NOT EXISTS users (
			id            TEXT PRIMARY KEY,
			github_id     TEXT UNIQUE  NOT NULL,
			username      TEXT         NOT NULL,
			email         TEXT         NOT NULL DEFAULT '',
			avatar_url    TEXT         NOT NULL DEFAULT '',
			role          TEXT         NOT NULL DEFAULT 'analyst',
			is_active     BOOLEAN      NOT NULL DEFAULT TRUE,
			last_login_at TIMESTAMPTZ,
			created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
		)`,

		`CREATE TABLE IF NOT EXISTS refresh_tokens (
			token      TEXT PRIMARY KEY,
			user_id    TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		// Stores OAuth state and PKCE challenge between redirect steps.
		`CREATE TABLE IF NOT EXISTS oauth_states (
			state          TEXT PRIMARY KEY,
			code_challenge TEXT        NOT NULL DEFAULT '',
			source         TEXT        NOT NULL DEFAULT 'web',
			redirect_uri   TEXT        NOT NULL DEFAULT '',
			created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		// One-time codes issued after GitHub callback, exchanged by CLI for tokens.
		`CREATE TABLE IF NOT EXISTS cli_auth_codes (
			code           TEXT PRIMARY KEY,
			user_id        TEXT        NOT NULL,
			access_token   TEXT        NOT NULL,
			refresh_token  TEXT        NOT NULL,
			code_challenge TEXT        NOT NULL DEFAULT '',
			expires_at     TIMESTAMPTZ NOT NULL,
			created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		`CREATE TABLE IF NOT EXISTS request_logs (
			id          BIGSERIAL PRIMARY KEY,
			method      TEXT         NOT NULL,
			path        TEXT         NOT NULL,
			status_code INT          NOT NULL,
			duration_ms BIGINT       NOT NULL,
			ip          TEXT         NOT NULL DEFAULT '',
			user_id     TEXT         NOT NULL DEFAULT '',
			created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
		)`,

		`CREATE INDEX IF NOT EXISTS idx_refresh_tokens_user_id ON refresh_tokens(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_created_at ON request_logs(created_at DESC)`,
	}

	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			// Surface the failing statement (truncated) to make debugging easy.
			preview := stmt
			if len(preview) > 60 {
				preview = preview[:60] + "..."
			}
			return fmt.Errorf("migration failed (%q): %w", preview, err)
		}
	}

	log.Println("database migrations complete")
	return nil
}

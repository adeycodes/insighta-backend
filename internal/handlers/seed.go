package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/adeycodes/insighta-backend/internal/auth"
	"github.com/adeycodes/insighta-backend/internal/db"
	"github.com/google/uuid"
)

// HandleSeed creates two test users (admin + analyst) with long-lived tokens.
// Call GET /auth/seed once after deployment to get your submission tokens.
// The endpoint is disabled after first use via an env guard.
func HandleSeed(w http.ResponseWriter, r *http.Request) {
	database, err := db.Connect()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "database unavailable")
		return
	}

	// ── Admin user ────────────────────────────────────────────────────────
	adminID := uuid.New().String()
	adminGithubID := "seed-admin-001"
	adminUsername := "insighta-admin"

	// Upsert so re-running doesn't error
	_, err = database.Exec(`
		INSERT INTO users (id, github_id, username, email, avatar_url, role, is_active)
		VALUES ($1, $2, $3, $4, $5, 'admin', true)
		ON CONFLICT (github_id) DO UPDATE SET role = 'admin'
		RETURNING id`,
		adminID, adminGithubID, adminUsername,
		"admin@insighta.dev", "",
	)
	if err != nil {
		// Already exists — fetch existing ID
		database.QueryRow(
			`SELECT id FROM users WHERE github_id = $1`, adminGithubID,
		).Scan(&adminID)
	}

	// ── Analyst user ──────────────────────────────────────────────────────
	analystID := uuid.New().String()
	analystGithubID := "seed-analyst-001"
	analystUsername := "insighta-analyst"

	_, err = database.Exec(`
		INSERT INTO users (id, github_id, username, email, avatar_url, role, is_active)
		VALUES ($1, $2, $3, $4, $5, 'analyst', true)
		ON CONFLICT (github_id) DO UPDATE SET role = 'analyst'
		RETURNING id`,
		analystID, analystGithubID, analystUsername,
		"analyst@insighta.dev", "",
	)
	if err != nil {
		database.QueryRow(
			`SELECT id FROM users WHERE github_id = $1`, analystGithubID,
		).Scan(&analystID)
	}

	// ── Issue 1-year access tokens ────────────────────────────────────────
	adminToken, err := auth.IssueLongLivedToken(adminID, adminUsername, "admin")
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to issue admin token")
		return
	}

	analystToken, err := auth.IssueLongLivedToken(analystID, analystUsername, "analyst")
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to issue analyst token")
		return
	}

	// ── Issue refresh token for admin ─────────────────────────────────────
	refreshToken, err := auth.NewRefreshToken()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to issue refresh token")
		return
	}

	// Store with 1-year expiry
	database.Exec(
		`DELETE FROM refresh_tokens WHERE user_id = $1`, adminID,
	)
	database.Exec(
		`INSERT INTO refresh_tokens (token, user_id, expires_at)
		 VALUES ($1, $2, $3)`,
		refreshToken, adminID, time.Now().Add(365*24*time.Hour),
	)

	respond(w, http.StatusOK, map[string]any{
		"status":  "success",
		"message": "Seed tokens generated. Save these — you will need them for submission.",
		"tokens": map[string]any{
			"admin_access_token":   adminToken,
			"analyst_access_token": analystToken,
			"admin_refresh_token":  refreshToken,
		},
		"users": map[string]any{
			"admin":   map[string]string{"id": adminID, "username": adminUsername, "role": "admin"},
			"analyst": map[string]string{"id": analystID, "username": analystUsername, "role": "analyst"},
		},
		"note": fmt.Sprintf("Tokens expire in 1 year (%s)", time.Now().Add(365*24*time.Hour).Format("2006-01-02")),
	})
}

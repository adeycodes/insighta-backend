package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/adeycodes/insighta-backend/internal/auth"
	"github.com/adeycodes/insighta-backend/internal/db"
	"github.com/adeycodes/insighta-backend/internal/middleware"
	"github.com/google/uuid"
)

// respond sends a JSON response.
func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	respond(w, status, map[string]string{"status": "error", "message": msg})
}

// ─── GET /auth/github ─────────────────────────────────────────────────────────
//
// Initiates the GitHub OAuth flow. Accepts optional PKCE and source params for
// CLI-originated logins.
//
// Query params:
//   source         = "cli" | "web"  (default: web)
//   code_challenge = BASE64URL(SHA256(verifier))
//   redirect_uri   = CLI callback URL (e.g. http://localhost:49152/callback)
//   state          = client-supplied state (CLI generates its own)

func HandleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	source := q.Get("source")
	if source != "cli" {
		source = "web"
	}

	// CLI may supply its own state; web gets a server-generated one.
	state := q.Get("state")
	if state == "" {
		var err error
		state, err = auth.NewState()
		if err != nil {
			apiError(w, http.StatusInternalServerError, "failed to generate state")
			return
		}
	}

	database, err := db.Connect()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "service unavailable")
		return
	}

	_, err = database.Exec(
		`INSERT INTO oauth_states (state, code_challenge, source, redirect_uri)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (state) DO UPDATE
		   SET code_challenge = EXCLUDED.code_challenge,
		       source         = EXCLUDED.source,
		       redirect_uri   = EXCLUDED.redirect_uri`,
		state,
		q.Get("code_challenge"),
		source,
		q.Get("redirect_uri"),
	)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to store oauth state")
		return
	}

	// Clean up states older than 10 minutes in the background.
	go database.Exec(`DELETE FROM oauth_states WHERE created_at < NOW() - INTERVAL '10 minutes'`)

	http.Redirect(w, r, auth.AuthorizeURL(state), http.StatusTemporaryRedirect)
}

// ─── GET /auth/github/callback ───────────────────────────────────────────────
//
// GitHub redirects here after the user authorises the app. We exchange the
// code for a GitHub token, look up or create the local user, then either:
//   - web:  set HTTP-only cookies and redirect to the portal dashboard
//   - cli:  issue a short-lived auth code and redirect to the CLI's local server

func HandleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := q.Get("code")
	state := q.Get("state")

	if code == "" || state == "" {
		apiError(w, http.StatusBadRequest, "missing code or state parameter")
		return
	}

	database, err := db.Connect()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "service unavailable")
		return
	}

	var source, codeChallenge, redirectURI string
	err = database.QueryRow(
		`SELECT source, code_challenge, redirect_uri FROM oauth_states WHERE state = $1`, state,
	).Scan(&source, &codeChallenge, &redirectURI)

	if err == sql.ErrNoRows {
		apiError(w, http.StatusBadRequest, "state is invalid or has expired")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "database error")
		return
	}

	database.Exec(`DELETE FROM oauth_states WHERE state = $1`, state)

	// Exchange the GitHub authorization code for a GitHub access token.
	githubToken, err := auth.ExchangeCode(code)
	if err != nil {
		apiError(w, http.StatusBadGateway, "failed to exchange code with GitHub")
		return
	}

	ghUser, err := auth.GetUser(githubToken)
	if err != nil || ghUser == nil {
		apiError(w, http.StatusBadGateway, "failed to fetch GitHub user info")
		return
	}

	userID, role, err := upsertUser(database, ghUser)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to create or update user")
		return
	}

	accessToken, err := auth.IssueAccessToken(userID, ghUser.Login, role)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to issue access token")
		return
	}

	refreshToken, err := auth.NewRefreshToken()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to generate refresh token")
		return
	}

	_, err = database.Exec(
		`INSERT INTO refresh_tokens (token, user_id, expires_at) VALUES ($1, $2, $3)`,
		refreshToken, userID, time.Now().Add(auth.RefreshTokenTTL),
	)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to persist refresh token")
		return
	}

	if source == "cli" {
		handleCLICallback(w, r, database, userID, accessToken, refreshToken, codeChallenge, redirectURI, state)
		return
	}

	handleWebCallback(w, r, accessToken, refreshToken)
}

// handleCLICallback issues a one-time auth code and redirects to the CLI's local server.
func handleCLICallback(w http.ResponseWriter, r *http.Request, database *sql.DB, userID, accessToken, refreshToken, codeChallenge, redirectURI, state string) {
	if redirectURI == "" {
		apiError(w, http.StatusBadRequest, "redirect_uri is required for CLI flows")
		return
	}

	authCode, err := auth.NewAuthCode()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to generate auth code")
		return
	}

	_, err = database.Exec(
		`INSERT INTO cli_auth_codes (code, user_id, access_token, refresh_token, code_challenge, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		authCode, userID, accessToken, refreshToken, codeChallenge, time.Now().Add(2*time.Minute),
	)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to persist auth code")
		return
	}

	target := fmt.Sprintf("%s?code=%s&state=%s", redirectURI, authCode, state)
	http.Redirect(w, r, target, http.StatusTemporaryRedirect)
}

// handleWebCallback redirects to the portal callback page with tokens as
// query params. The plain HTML portal reads them and stores in localStorage.
func handleWebCallback(w http.ResponseWriter, r *http.Request, accessToken, refreshToken string) {
	portal := os.Getenv("WEB_PORTAL_URL")
	if portal == "" {
		portal = "http://localhost:5500"
	}
	target := fmt.Sprintf(
		"%s/callback.html?token=%s&refresh_token=%s",
		portal,
		url.QueryEscape(accessToken),
		url.QueryEscape(refreshToken),
	)
	http.Redirect(w, r, target, http.StatusTemporaryRedirect)
}

// placeholder_end

// ─── POST /auth/exchange ─────────────────────────────────────────────────────
//
// CLI-only. Exchanges a one-time auth code and PKCE verifier for a token pair.

func HandleExchange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Code == "" {
		apiError(w, http.StatusBadRequest, "request must include code and code_verifier")
		return
	}

	database, err := db.Connect()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "service unavailable")
		return
	}

	var accessToken, refreshToken, codeChallenge string
	var expiresAt time.Time

	err = database.QueryRow(
		`SELECT access_token, refresh_token, code_challenge, expires_at
		 FROM cli_auth_codes WHERE code = $1`, body.Code,
	).Scan(&accessToken, &refreshToken, &codeChallenge, &expiresAt)

	if err == sql.ErrNoRows {
		apiError(w, http.StatusUnauthorized, "auth code is invalid or has already been used")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "database error")
		return
	}
	if time.Now().After(expiresAt) {
		database.Exec(`DELETE FROM cli_auth_codes WHERE code = $1`, body.Code)
		apiError(w, http.StatusUnauthorized, "auth code has expired — please run insighta login again")
		return
	}

	// PKCE check — only required when a challenge was registered.
	if codeChallenge != "" && !auth.VerifyPKCE(body.CodeVerifier, codeChallenge) {
		apiError(w, http.StatusUnauthorized, "PKCE verification failed")
		return
	}

	// One-time use: delete immediately after a successful exchange.
	database.Exec(`DELETE FROM cli_auth_codes WHERE code = $1`, body.Code)

	respond(w, http.StatusOK, map[string]any{
		"status":        "success",
		"access_token":  accessToken,
		"refresh_token": refreshToken,
	})
}

// ─── POST /auth/refresh ──────────────────────────────────────────────────────
//
// Issues a new access/refresh token pair in exchange for a valid refresh token.
// CLI sends the token in the request body; the web portal sends it via cookie.

func HandleRefresh(w http.ResponseWriter, r *http.Request) {
	refreshToken := refreshTokenFromRequest(r)
	if refreshToken == "" {
		apiError(w, http.StatusBadRequest, "refresh token is required")
		return
	}

	database, err := db.Connect()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "service unavailable")
		return
	}

	var userID string
	var expiresAt time.Time

	err = database.QueryRow(
		`SELECT user_id, expires_at FROM refresh_tokens WHERE token = $1`, refreshToken,
	).Scan(&userID, &expiresAt)

	if err == sql.ErrNoRows {
		apiError(w, http.StatusUnauthorized, "refresh token is invalid")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "database error")
		return
	}
	if time.Now().After(expiresAt) {
		database.Exec(`DELETE FROM refresh_tokens WHERE token = $1`, refreshToken)
		apiError(w, http.StatusUnauthorized, "refresh token has expired — please log in again")
		return
	}

	// Invalidate immediately — refresh tokens are single-use (rotation).
	database.Exec(`DELETE FROM refresh_tokens WHERE token = $1`, refreshToken)

	var username, role string
	var isActive bool
	err = database.QueryRow(
		`SELECT username, role, is_active FROM users WHERE id = $1`, userID,
	).Scan(&username, &role, &isActive)

	if err != nil || !isActive {
		apiError(w, http.StatusForbidden, "account not found or has been disabled")
		return
	}

	newAccess, err := auth.IssueAccessToken(userID, username, role)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to issue access token")
		return
	}

	newRefresh, err := auth.NewRefreshToken()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to generate refresh token")
		return
	}

	database.Exec(
		`INSERT INTO refresh_tokens (token, user_id, expires_at) VALUES ($1, $2, $3)`,
		newRefresh, userID, time.Now().Add(auth.RefreshTokenTTL),
	)

	// If this was a web request, update the cookies.
	if _, err := r.Cookie("insighta_session"); err == nil {
		secure := os.Getenv("ENV") == "production"
		http.SetCookie(w, &http.Cookie{
			Name: "insighta_session", Value: newAccess,
			Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
			MaxAge: int(auth.AccessTokenTTL.Seconds()),
		})
		http.SetCookie(w, &http.Cookie{
			Name: "insighta_refresh", Value: newRefresh,
			Path: "/auth/refresh", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
			MaxAge: int(auth.RefreshTokenTTL.Seconds()),
		})
	}

	respond(w, http.StatusOK, map[string]any{
		"status":        "success",
		"access_token":  newAccess,
		"refresh_token": newRefresh,
	})
}

// ─── POST /auth/logout ───────────────────────────────────────────────────────

func HandleLogout(w http.ResponseWriter, r *http.Request) {
	refreshToken := refreshTokenFromRequest(r)

	if refreshToken != "" {
		if database, err := db.Connect(); err == nil {
			database.Exec(`DELETE FROM refresh_tokens WHERE token = $1`, refreshToken)
		}
	}

	// Expire all cookies regardless of how the request arrived.
	for _, name := range []string{"insighta_session", "insighta_refresh", "csrf_token"} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", MaxAge: -1, Path: "/"})
	}

	respond(w, http.StatusOK, map[string]string{"status": "success", "message": "logged out"})
}

// ─── GET /auth/me ─────────────────────────────────────────────────────────────

func HandleMe(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r)
	if user == nil {
		apiError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	database, err := db.Connect()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "service unavailable")
		return
	}

	var email, avatarURL string
	var lastLoginAt *time.Time
	database.QueryRow(
		`SELECT email, avatar_url, last_login_at FROM users WHERE id = $1`, user.ID,
	).Scan(&email, &avatarURL, &lastLoginAt)

	respond(w, http.StatusOK, map[string]any{
		"status": "success",
		"data": map[string]any{
			"id":            user.ID,
			"username":      user.Username,
			"email":         email,
			"avatar_url":    avatarURL,
			"role":          user.Role,
			"last_login_at": lastLoginAt,
		},
	})
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// refreshTokenFromRequest extracts a refresh token from the request body or cookie.
func refreshTokenFromRequest(r *http.Request) string {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	// Try body first (CLI).
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body.RefreshToken != "" {
		return body.RefreshToken
	}
	// Fall back to cookie (web).
	if c, err := r.Cookie("insighta_refresh"); err == nil {
		return c.Value
	}
	return ""
}

// upsertUser creates a new user row or updates the existing one on login.
func upsertUser(database *sql.DB, ghUser *auth.GitHubUser) (userID, role string, err error) {
	err = database.QueryRow(
		`SELECT id, role FROM users WHERE github_id = $1`, fmt.Sprint(ghUser.ID),
	).Scan(&userID, &role)

	if err == sql.ErrNoRows {
		userID = uuid.New().String()
		role = string(roleAnalyst)

		_, err = database.Exec(
			`INSERT INTO users (id, github_id, username, email, avatar_url, role, last_login_at)
			 VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
			userID, fmt.Sprint(ghUser.ID), ghUser.Login, ghUser.Email, ghUser.AvatarURL, role,
		)
		return
	}

	if err != nil {
		return "", "", err
	}

	// Update mutable fields on every login.
	database.Exec(
		`UPDATE users SET username = $1, email = $2, avatar_url = $3, last_login_at = NOW() WHERE id = $4`,
		ghUser.Login, ghUser.Email, ghUser.AvatarURL, userID,
	)
	return userID, role, nil
}

// roleAnalyst is a local alias to avoid importing models from handlers.
const roleAnalyst = "analyst"

package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/adeycodes/insighta-backend/internal/auth"
	"github.com/adeycodes/insighta-backend/internal/db"
	"github.com/adeycodes/insighta-backend/internal/models"
	"golang.org/x/time/rate"
)

// respond writes a JSON body with the given status code.
func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	respond(w, status, map[string]string{"status": "error", "message": msg})
}

// ─── CORS ────────────────────────────────────────────────────────────────────

// CORS sets permissive headers for the web portal origin and passes preflight.
func CORS(next http.Handler) http.Handler {
	webOrigin := os.Getenv("WEB_PORTAL_URL")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		if origin != "" {
			if origin == webOrigin {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}

		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Version, X-CSRF-Token")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ─── RequireAuth ─────────────────────────────────────────────────────────────

// RequireAuth validates the JWT from either the Authorization header (CLI)
// or the insighta_session HTTP-only cookie (web portal).
func RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := tokenFromRequest(r)
		if raw == "" {
			apiError(w, http.StatusUnauthorized, "authentication required")
			return
		}

		claims, err := auth.ParseAccessToken(raw)
		if err != nil {
			apiError(w, http.StatusUnauthorized, "token is invalid or has expired")
			return
		}

		// Confirm the account is still active before serving the request.
		database, err := db.Connect()
		if err != nil {
			apiError(w, http.StatusInternalServerError, "service unavailable")
			return
		}

		var isActive bool
		if err := database.QueryRow(`SELECT is_active FROM users WHERE id = $1`, claims.UserID).Scan(&isActive); err != nil {
			apiError(w, http.StatusUnauthorized, "account not found")
			return
		}
		if !isActive {
			apiError(w, http.StatusForbidden, "account has been disabled")
			return
		}

		user := &models.User{
			ID:       claims.UserID,
			Username: claims.Username,
			Role:     models.Role(claims.Role),
		}
		ctx := context.WithValue(r.Context(), models.UserCtxKey, user)
		next(w, r.WithContext(ctx))
	}
}

// tokenFromRequest extracts a bearer token from the Authorization header,
// falling back to the insighta_session cookie for browser-originated requests.
func tokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie("insighta_session"); err == nil {
		return c.Value
	}
	return ""
}

// UserFromContext returns the authenticated user stored in the request context.
func UserFromContext(r *http.Request) *models.User {
	u, _ := r.Context().Value(models.UserCtxKey).(*models.User)
	return u
}

// ─── RequireRole ─────────────────────────────────────────────────────────────

// RequireRole enforces that the authenticated user holds the specified role.
// Must be chained after RequireAuth.
func RequireRole(role models.Role) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			user := UserFromContext(r)
			if user == nil {
				apiError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			if user.Role != role {
				apiError(w, http.StatusForbidden, fmt.Sprintf("this action requires the %s role", role))
				return
			}
			next(w, r)
		}
	}
}

// ─── RequireAPIVersion ───────────────────────────────────────────────────────

// RequireAPIVersion rejects requests that don't include the X-API-Version: 1 header.
func RequireAPIVersion(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Version") != "1" {
			apiError(w, http.StatusBadRequest, "missing or unsupported X-API-Version header — expected: 1")
			return
		}
		next(w, r)
	}
}

// ─── RateLimit ───────────────────────────────────────────────────────────────

type rateBucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

var (
	rateBuckets = make(map[string]*rateBucket)
	rateMu      sync.Mutex
)

func init() {
	// Evict stale buckets every minute to prevent unbounded memory growth.
	go func() {
		for range time.Tick(time.Minute) {
			rateMu.Lock()
			for ip, b := range rateBuckets {
				if time.Since(b.lastSeen) > 3*time.Minute {
					delete(rateBuckets, ip)
				}
			}
			rateMu.Unlock()
		}
	}()
}

func bucketForIP(ip string) *rate.Limiter {
	rateMu.Lock()
	defer rateMu.Unlock()

	b, ok := rateBuckets[ip]
	if !ok {
		// Allow 60 requests per minute with a burst of 10.
		b = &rateBucket{limiter: rate.NewLimiter(rate.Every(time.Second), 10)}
		rateBuckets[ip] = b
	}
	b.lastSeen = time.Now()
	return b.limiter
}

// RateLimit applies per-IP rate limiting. Fly.io/proxies set X-Forwarded-For.
func RateLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := realIP(r)
		if !bucketForIP(ip).Allow() {
			apiError(w, http.StatusTooManyRequests, "too many requests — please slow down")
			return
		}
		next(w, r)
	}
}

func realIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		// X-Forwarded-For can be a comma-separated list; the first is the client.
		return strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// ─── RequestLogger ───────────────────────────────────────────────────────────

// responseWriter wraps http.ResponseWriter to capture the status code after the
// handler writes it, since WriteHeader can only be called once.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

// RequestLogger logs every request to stdout and persists it to the DB.
func RequestLogger(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		next(rw, r)

		duration := time.Since(started).Milliseconds()
		userID := ""
		if u := UserFromContext(r); u != nil {
			userID = u.ID
		}

		log.Printf(`method=%s path=%s status=%d duration_ms=%d ip=%s user_id=%s`,
			r.Method, r.URL.Path, rw.status, duration, realIP(r), userID)

		// Persist asynchronously so logging never blocks a response.
		go func() {
			database, err := db.Connect()
			if err != nil {
				return
			}
			database.Exec(
				`INSERT INTO request_logs (method, path, status_code, duration_ms, ip, user_id)
				 VALUES ($1, $2, $3, $4, $5, $6)`,
				r.Method, r.URL.Path, rw.status, duration, realIP(r), userID,
			)
		}()
	}
}

// ─── CSRF ────────────────────────────────────────────────────────────────────

// ValidateCSRF implements the Double Submit Cookie pattern for browser clients.
// CLI requests carry an Authorization header and bypass this check.
func ValidateCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Safe methods and CLI requests (Bearer token) don't need CSRF protection.
		if r.Method == http.MethodGet || r.Method == http.MethodOptions {
			next(w, r)
			return
		}
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			next(w, r)
			return
		}

		headerToken := r.Header.Get("X-CSRF-Token")
		cookie, err := r.Cookie("csrf_token")
		if err != nil || headerToken == "" || headerToken != cookie.Value {
			apiError(w, http.StatusForbidden, "CSRF validation failed")
			return
		}

		next(w, r)
	}
}

// Chain applies middleware functions from left to right (first listed = outermost).
func Chain(h http.HandlerFunc, mws ...func(http.HandlerFunc) http.HandlerFunc) http.HandlerFunc {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

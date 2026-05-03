package main

import (
	"log"
	"net/http"
	"os"

	"github.com/adeycodes/insighta-backend/internal/db"
	"github.com/adeycodes/insighta-backend/internal/handlers"
	"github.com/adeycodes/insighta-backend/internal/middleware"
	"github.com/adeycodes/insighta-backend/internal/models"
)

func main() {
	// Fail fast on missing required env vars.
	for _, env := range []string{"DATABASE_URL", "JWT_SECRET", "GITHUB_CLIENT_ID", "GITHUB_CLIENT_SECRET", "BACKEND_URL"} {
		if os.Getenv(env) == "" {
			log.Fatalf("required environment variable %q is not set", env)
		}
	}

	database, err := db.Connect()
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}

	if err := db.Migrate(database); err != nil {
		log.Fatalf("database migration failed: %v", err)
	}

	mux := http.NewServeMux()
	registerRoutes(mux)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("insighta backend listening on :%s", port)
	if err := http.ListenAndServe(":"+port, middleware.CORS(mux)); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func registerRoutes(mux *http.ServeMux) {
	// Shorthand middleware chains.
	// auth     = rate-limited + logged + authenticated
	// admin    = auth + admin role
	// apiV1    = auth + API version header
	// adminAPI = apiV1 + admin role

	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return middleware.Chain(h,
			middleware.RateLimit,
			middleware.RequestLogger,
			middleware.RequireAuth,
		)
	}

	apiV1 := func(h http.HandlerFunc) http.HandlerFunc {
		return middleware.Chain(h,
			middleware.RateLimit,
			middleware.RequestLogger,
			middleware.RequireAuth,
			middleware.RequireAPIVersion,
		)
	}

	adminAPI := func(h http.HandlerFunc) http.HandlerFunc {
		return middleware.Chain(h,
			middleware.RateLimit,
			middleware.RequestLogger,
			middleware.ValidateCSRF,
			middleware.RequireAuth,
			middleware.RequireAPIVersion,
			middleware.RequireRole(models.RoleAdmin),
		)
	}

	// ─── Auth routes ──────────────────────────────────────────────────────
	mux.HandleFunc("GET /auth/github", handlers.HandleGitHubLogin)
	mux.HandleFunc("GET /auth/github/callback", handlers.HandleGitHubCallback)
	mux.HandleFunc("POST /auth/exchange", handlers.HandleExchange)   // CLI only
	mux.HandleFunc("POST /auth/refresh", handlers.HandleRefresh)
	mux.HandleFunc("POST /auth/logout", handlers.HandleLogout)
	mux.HandleFunc("GET /auth/me", auth(handlers.HandleMe))

	// ─── Profile routes ───────────────────────────────────────────────────
	// Order matters: more specific paths first to avoid /api/profiles catching /api/profiles/search.
	mux.HandleFunc("GET /api/profiles/search", apiV1(handlers.HandleSearchProfiles))
	mux.HandleFunc("GET /api/profiles/export", apiV1(handlers.HandleExportCSV))
	mux.HandleFunc("GET /api/profiles/{id}", apiV1(handlers.HandleGetProfile))
	mux.HandleFunc("DELETE /api/profiles/{id}", adminAPI(handlers.HandleDeleteProfile))
	mux.HandleFunc("GET /api/profiles", apiV1(handlers.HandleListProfiles))
	mux.HandleFunc("POST /api/profiles", adminAPI(handlers.HandleCreateProfile))

	// ─── Health check ─────────────────────────────────────────────────────
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
}

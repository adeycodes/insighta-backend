package handler

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/adeycodes/insighta-backend/internal/db"
	"github.com/adeycodes/insighta-backend/internal/handlers"
	"github.com/adeycodes/insighta-backend/internal/middleware"
	"github.com/adeycodes/insighta-backend/internal/models"
)

var (
	mux  *http.ServeMux
	once sync.Once
)

func Handler(w http.ResponseWriter, r *http.Request) {
	once.Do(bootstrap)
	middleware.CORS(mux).ServeHTTP(w, r)
}

func bootstrap() {
	for _, key := range []string{"DATABASE_URL", "JWT_SECRET", "GITHUB_CLIENT_ID", "GITHUB_CLIENT_SECRET", "BACKEND_URL"} {
		if os.Getenv(key) == "" {
			log.Fatalf("missing required environment variable: %s", key)
		}
	}

	database, err := db.Connect()
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	if err := db.Migrate(database); err != nil {
		log.Fatalf("migration failed: %v", err)
	}

	mux = http.NewServeMux()
	registerRoutes(mux)
}

func registerRoutes(mux *http.ServeMux) {
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

	// ─── Health & diagnostics ─────────────────────────────────────────────

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("GET /db-check", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		database, err := db.Connect()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"status":"error","stage":"connect","message":"` + err.Error() + `"}`))
			return
		}

		if err := database.Ping(); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"status":"error","stage":"ping","message":"` + err.Error() + `"}`))
			return
		}

		var count int
		database.QueryRow("SELECT COUNT(*) FROM profiles").Scan(&count)

		w.Write([]byte(`{"status":"ok","database":"connected","profiles":` + strconv.Itoa(count) + `}`))
	})

	// ─── Auth ─────────────────────────────────────────────────────────────

	mux.HandleFunc("GET /auth/github", handlers.HandleGitHubLogin)
	mux.HandleFunc("GET /auth/github/callback", handlers.HandleGitHubCallback)
	mux.HandleFunc("POST /auth/exchange", handlers.HandleExchange)
	mux.HandleFunc("POST /auth/refresh", handlers.HandleRefresh)
	mux.HandleFunc("POST /auth/logout", handlers.HandleLogout)
	mux.HandleFunc("GET /auth/me", auth(handlers.HandleMe))

	// ─── Profiles ─────────────────────────────────────────────────────────

	mux.HandleFunc("GET /api/profiles/search", apiV1(handlers.HandleSearchProfiles))
	mux.HandleFunc("GET /api/profiles/export", apiV1(handlers.HandleExportCSV))
	mux.HandleFunc("GET /api/profiles/{id}", apiV1(handlers.HandleGetProfile))
	mux.HandleFunc("DELETE /api/profiles/{id}", adminAPI(handlers.HandleDeleteProfile))
	mux.HandleFunc("GET /api/profiles", apiV1(handlers.HandleListProfiles))
	mux.HandleFunc("POST /api/profiles", adminAPI(handlers.HandleCreateProfile))
	mux.HandleFunc("GET /auth/seed", handlers.HandleSeed)
}

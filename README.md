# Insighta Labs+ — Backend

A Go HTTP server powering the Insighta Labs+ Profile Intelligence System. It serves both the CLI and the web portal from a single source of truth.

---

## System Architecture

```
┌─────────────────────┐       GitHub OAuth       ┌─────────────────────────┐
│  insighta CLI (Go)  │──── PKCE + Bearer JWT ──▶│                         │
│  cobra + tablewriter│◀─── access/refresh ──────│   insighta-backend      │
└─────────────────────┘                          │   (Go net/http)         │
                                                 │                         │
┌─────────────────────┐      HTTP-only cookies   │   PostgreSQL            │
│  insighta-web       │──── CSRF + session ────▶│   (shared DB)           │
│  (SvelteKit SSR)    │◀─── Set-Cookie ──────────│                         │
└─────────────────────┘                          └─────────────────────────┘
```

The backend is a standard Go `net/http` server deployed on Fly.io. The CLI and web portal share identical API endpoints — the only difference is how credentials are transported (Bearer header vs HTTP-only cookie).

**Directory layout:**

```
insighta-backend/
├── cmd/server/main.go              # Entry point and route registration
├── internal/
│   ├── auth/
│   │   ├── github.go               # GitHub OAuth URL and user fetch
│   │   ├── jwt.go                  # Access/refresh token issuance
│   │   └── pkce.go                 # PKCE challenge verification
│   ├── db/
│   │   └── db.go                   # Connection pool and migrations
│   ├── handlers/
│   │   ├── auth.go                 # OAuth, exchange, refresh, logout, me
│   │   └── profiles.go             # CRUD, search, export
│   ├── middleware/
│   │   └── middleware.go           # Auth, RBAC, versioning, rate limit, CSRF, CORS
│   └── models/
│       └── models.go               # Shared struct definitions
├── Dockerfile
├── fly.toml
└── .env.example
```

---

## Auth Flow

### Web portal flow (browser)

```
Browser           Backend                  GitHub
  │                  │                        │
  │── GET /auth/github?source=web ──────────▶│
  │                  │── generate state ──────│
  │                  │   store in oauth_states│
  │◀── 302 redirect ─│── github.com/login?... │
  │                                           │
  │─────────── user approves ────────────────▶│
  │◀── 302 /auth/github/callback?code=... ───│
  │                  │                        │
  │─── GET /auth/github/callback ───────────▶│
  │                  │── ExchangeCode(code) ─▶│
  │                  │◀── github_access_token ─│
  │                  │── GET /user ───────────▶│
  │                  │◀── {login, email, ...} ─│
  │                  │── upsert user in DB     │
  │                  │── issue JWT pair        │
  │◀── 302 /dashboard (Set-Cookie: insighta_session, insighta_refresh, csrf_token)
```

### CLI flow (PKCE)

```
CLI                 Backend                  GitHub
 │                     │                        │
 │ 1. NewPKCE()        │                        │
 │    verifier (secret)│                        │
 │    challenge = BASE64URL(SHA256(verifier))   │
 │                     │                        │
 │ 2. Start localhost:PORT/callback             │
 │                     │                        │
 │ 3. GET /auth/github?source=cli               │
 │    &code_challenge=CHALLENGE                 │
 │    &redirect_uri=http://localhost:PORT/callback
 │    &state=RANDOM_STATE ────────────────────▶│
 │                     │── store state + challenge
 │                     │── redirect to GitHub ─▶│
 │                                              │
 │── user approves in browser ─────────────────▶│
 │◀─── /auth/github/callback?code=GITHUB_CODE ──│
 │                     │── ExchangeCode(code)   │
 │                     │── GetUser(github_token)│
 │                     │── upsert user          │
 │                     │── issue JWT pair        │
 │                     │── generate auth_code   │
 │                     │── store in cli_auth_codes
 │◀── 302 localhost:PORT/callback?code=AUTH_CODE│
 │                     │                        │
 │ 4. POST /auth/exchange                       │
 │    {code: AUTH_CODE, code_verifier: VERIFIER}│
 │                     │── verify PKCE          │
 │                     │   SHA256(verifier) == challenge?
 │                     │── delete auth_code (single-use)
 │◀── {access_token, refresh_token} ────────────│
 │                     │                        │
 │ 5. Save to ~/.insighta/credentials.json      │
```

---

## Token Handling

| Token | TTL | Transport |
|---|---|---|
| Access token (JWT) | 3 minutes | CLI: `Authorization: Bearer ...` / Web: `insighta_session` HTTP-only cookie |
| Refresh token (opaque) | 5 minutes | CLI: stored in `credentials.json` / Web: `insighta_refresh` HTTP-only cookie (scoped to `/auth/refresh`) |

**Refresh strategy:**

- Access tokens expire in 3 minutes. Both clients must proactively call `POST /auth/refresh`.
- Refresh tokens are **single-use**. The old token is deleted the moment it is exchanged, and a new pair is issued.
- If the refresh token has also expired, the user must log in again.
- The CLI does this automatically and silently. If the refresh also fails, it prints `session expired — please run: insighta login`.
- The web portal handles this in `hooks.server.ts` on every request.

---

## Role Enforcement

Two roles exist: `admin` and `analyst`. All new users are created as `analyst`.

To promote a user to `admin`, update their row in the database directly:

```sql
UPDATE users SET role = 'admin' WHERE username = 'adeycodes';
```

**Endpoint access matrix:**

| Endpoint | analyst | admin |
|---|---|---|
| `GET /api/profiles` | ✓ | ✓ |
| `GET /api/profiles/:id` | ✓ | ✓ |
| `GET /api/profiles/search` | ✓ | ✓ |
| `GET /api/profiles/export` | ✓ | ✓ |
| `POST /api/profiles` | ✗ | ✓ |
| `DELETE /api/profiles/:id` | ✗ | ✓ |

Role is embedded in the JWT at login time. The `RequireRole` middleware reads it from the request context (set by `RequireAuth`) and returns 403 if it doesn't match.

---

## API Versioning

Every `/api/*` endpoint requires the `X-API-Version: 1` header. Requests without it receive:

```json
{ "status": "error", "message": "missing or unsupported X-API-Version header — expected: 1" }
```

This is enforced by the `RequireAPIVersion` middleware, which is chained onto all profile routes.

---

## Pagination Shape (v1)

```json
{
  "status": "success",
  "page": 1,
  "limit": 10,
  "total": 2048,
  "total_pages": 205,
  "links": {
    "self": "/api/profiles?page=1&limit=10",
    "next": "/api/profiles?page=2&limit=10",
    "prev": null
  },
  "data": [...]
}
```

---

## Natural Language Parsing

The `GET /api/profiles/search?q=` endpoint accepts free-text queries. The parser in `handlers/profiles.go` works by:

1. Lowercasing and tokenising the query into words.
2. Scanning for gender signals (`male`, `males`, `man`, `men`, `boy`, etc.)
3. Scanning for age group signals (`child`, `teen`, `adult`, `senior`, `young`, etc.)
4. Scanning for numeric bounds (`above 30`, `under 25`, `between 20 and 35`)
5. Scanning for country names/codes (`nigeria` → `NG`, `south africa` → `ZA`, etc.)
6. Building a SQL WHERE clause dynamically from matched signals.

If no signals are found, a 422 is returned with a helpful message.

Example queries:
- `"young males from Nigeria"` → gender=male, age 16–24, country_id=NG
- `"adult women in Kenya"` → gender=female, age_group=adult, country_id=KE
- `"seniors above 65"` → age >= 65
- `"teenagers between 14 and 17"` → age >= 14, age <= 17

---

## CLI Usage

```bash
# Install
go install github.com/adeycodes/insighta-cli@latest

# Login (opens browser)
insighta login

# Show current user
insighta whoami

# List profiles
insighta profiles list
insighta profiles list --gender male --country-id NG --sort-by age --order asc

# Get a single profile
insighta profiles get <id>

# Natural language search
insighta profiles search "young females from Ghana"

# Create profile (admin only)
insighta profiles create "Amara Okonkwo"

# Export to CSV
insighta profiles export
insighta profiles export --gender female --output women.csv

# Logout
insighta logout
```

---

## Environment Variables

| Variable | Required | Description |
|---|---|---|
| `DATABASE_URL` | ✓ | PostgreSQL connection string |
| `JWT_SECRET` | ✓ | Secret used to sign JWTs — must be long and random |
| `GITHUB_CLIENT_ID` | ✓ | GitHub OAuth App client ID |
| `GITHUB_CLIENT_SECRET` | ✓ | GitHub OAuth App client secret |
| `BACKEND_URL` | ✓ | Public URL of this backend (used in OAuth callback) |
| `WEB_PORTAL_URL` | ✓ | Public URL of the web portal (used for CORS and redirects) |
| `ENV` | ✓ | `production` enables Secure flag on cookies |
| `PORT` | — | HTTP listen port (default: 8080) |

---

## Deployment (Fly.io)

```bash
# Install Fly CLI
curl -L https://fly.io/install.sh | sh

# Create the app
fly launch --name insighta-backend --region lhr --no-deploy

# Set secrets
fly secrets set \
  DATABASE_URL="postgres://..." \
  JWT_SECRET="$(openssl rand -hex 32)" \
  GITHUB_CLIENT_ID="..." \
  GITHUB_CLIENT_SECRET="..." \
  BACKEND_URL="https://insighta-backend.fly.dev" \
  WEB_PORTAL_URL="https://insighta.vercel.app" \
  ENV="production"

# Deploy
fly deploy
```

Set your GitHub OAuth App's callback URL to:
```
https://insighta-backend.fly.dev/auth/github/callback
```

---

## Rate Limiting

Each IP is limited to a burst of 10 requests, replenished at 10 requests per second (60/min sustained). This is enforced by the `RateLimit` middleware using a token bucket algorithm (`golang.org/x/time/rate`). Stale buckets are evicted every minute to prevent memory growth.

Fly.io's proxy sets `X-Forwarded-For`, which the middleware reads to identify the real client IP behind the proxy.

---

## Live URLs

- **Backend:** https://insighta-backend.fly.dev
- **Web Portal:** https://insighta-web.vercel.app
- **CLI:** `go install github.com/adeycodes/insighta-cli@latest`

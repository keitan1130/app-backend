# app-back

Go + PostgreSQL backend for a shared 32x32 pixel grid.

## Endpoints

Both prefixes are available:

- /api/v1/* (canonical)
- /api/* (compatibility alias)

Implemented:

- GET /healthz
- GET /readyz
- GET /grid?id=global
- POST /cell

## Local run (Docker)

0. Create env file:
   cp .env.example .env
   # set POSTGRES_PASSWORD to a strong random value before first start
1. Start containers:
   docker compose up -d --build
2. Check health:
   curl -s http://localhost:8080/api/v1/healthz
3. Read grid:
   curl -s "http://localhost:8080/api/v1/grid?id=global"

## Example update

curl -s -X POST "http://localhost:8080/api/v1/cell" \
  -H "Content-Type: application/json" \
  -d '{
    "id":"global",
    "index":513,
    "color":"#111827",
    "if_match_version":0
  }'

## Environment variables

- PORT (app default: 8080, but docker compose now requires explicit value in .env)
- DATABASE_URL (app default points to db service in compose)
- CORS_ALLOWED_ORIGINS (app default: https://app.keitan1130.com,http://localhost:5173, but docker compose requires explicit value in .env)
- TRUSTED_PROXY_CIDRS (comma-separated CIDRs. Forwarded IP headers are trusted only when RemoteAddr is in this list)
- MARKITDOWN_TIMEOUT_SECONDS (integer seconds for conversion timeout, default: 30)

For Docker compose, set `POSTGRES_DB`, `POSTGRES_USER`, `POSTGRES_PASSWORD`, `PORT`, and `CORS_ALLOWED_ORIGINS` in `.env`.
Do not commit `.env` to Git.

## Notes for Cloudflare Tunnel

Keep your existing Cloudflare Tunnel route to this backend origin (for example localhost:8080 on the host where docker compose runs).
No application changes are required if tunnel routing is already configured for api.keitan1130.com.

## MarkItDown supported formats

This backend calls MarkItDown via CLI (`markitdown <file>`).

- File upload: PDF / PowerPoint / Word / Excel / HTML / CSV / JSON / XML
- Manual input (`application/json` with `input`): HTML / CSV / JSON / XML text only
- URL, image, audio, ZIP, EPub inputs are rejected

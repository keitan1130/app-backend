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

- PORT (default: 8080)
- DATABASE_URL (default points to db service in compose)
- CORS_ALLOWED_ORIGINS (comma-separated, default: https://app.keitan1130.com,http://localhost:5173)
- TRUSTED_PROXY_CIDRS (comma-separated CIDRs. Forwarded IP headers are trusted only when RemoteAddr is in this list)
- MARKITDOWN_TIMEOUT_SECONDS (integer seconds for conversion timeout, default: 30)

## Notes for Cloudflare Tunnel

Keep your existing Cloudflare Tunnel route to this backend origin (for example localhost:8080 on the host where docker compose runs).
No application changes are required if tunnel routing is already configured for api.keitan1130.com.

## MarkItDown supported formats

This backend calls MarkItDown via CLI (`markitdown <file>`).

- File upload: PDF / PowerPoint / Word / Excel / HTML / CSV / JSON / XML
- Manual input (`application/json` with `input`): HTML / CSV / JSON / XML text only
- URL, image, audio, ZIP, EPub inputs are rejected

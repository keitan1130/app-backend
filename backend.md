# Backend Specification for Shared Pixel Grid

This document defines the backend contract for the frontend in this repository.
Target use case: one globally shared 32x32 pixel board.

## 1. Scope and Goals

- Provide a minimal and robust API for a shared grid.
- Allow many clients to read and update the same board.
- Prevent lost updates with optimistic concurrency control.
- Keep implementation simple for first release.

## 2. Non-Goals (Current Version)

- Authentication and user accounts
- Room-based multi-board support
- Real-time push transport (WebSocket/SSE)
- Edit history and undo/redo

## 3. Base URL and CORS

- Production API origin: `https://api.keitan1130.com`
- Frontend origin allowed by CORS: `https://app.keitan1130.com`
- Local development origin allowed by CORS: `http://localhost:5173`

CORS requirements:

- Allowed methods: `GET`, `POST`, `OPTIONS`
- Allowed headers: `Content-Type`
- Credentials: not required (do not enable unless needed)

API path policy for current release:

- Primary path: `/api/v1/*`
- Compatibility alias: `/api/*` (same behavior and response body)
- Current frontend uses `/api/v1/*`.
- Keep `/api/*` alias enabled during migration/rollback window.

## 4. Data Model

### 4.1 Logical Model

A single canvas identified by `id = "global"`.

- `grid_size`: integer (`32` for current release)
- `cells`: array of color strings (`#RRGGBB`), length = `grid_size * grid_size`
- `version`: monotonically increasing integer
- `updated_at`: UTC timestamp

### 4.2 PostgreSQL Table

```sql
CREATE TABLE IF NOT EXISTS canvases (
  id TEXT PRIMARY KEY,
  grid_size INTEGER NOT NULL CHECK (grid_size > 0),
  cells JSONB NOT NULL,
  version BIGINT NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

### 4.3 Initial Seed

```sql
INSERT INTO canvases (id, grid_size, cells, version)
VALUES (
  'global',
  32,
  to_jsonb(ARRAY(SELECT '#FFFFFF' FROM generate_series(1, 1024))),
  0
)
ON CONFLICT (id) DO NOTHING;
```

## 5. API Contract

All endpoints below are defined for both:

- `/api/v1/...` (canonical)
- `/api/...` (compatibility alias)

## 5.1 GET /api/v1/healthz

Purpose: process health check.

Response `200`:

```json
{
  "ok": true
}
```

Notes:

- Must not depend on database availability.

## 5.2 GET /api/v1/readyz

Purpose: readiness check including database connectivity.

Response `200`:

```json
{
  "ok": true,
  "db": "up"
}
```

Response `503`:

```json
{
  "ok": false,
  "error": "db_unavailable"
}
```

Notes:

- This endpoint must verify a real DB query (for example `SELECT 1`).

## 5.3 GET /api/v1/grid

Purpose: fetch current board state.

Query parameters:

- `id` (optional): canvas id, default `global`

Response `200`:

```json
{
  "id": "global",
  "grid_size": 32,
  "cells": ["#FFFFFF", "#111827"],
  "version": 42,
  "updated_at": "2026-04-10T12:34:56Z"
}
```

Validation requirements:

- `cells.length === grid_size * grid_size`
- each cell matches regex: `^#[0-9A-F]{6}$`
- backend should return uppercase hex

Errors:

- `404` when canvas not found
- `500` for internal errors

## 5.4 POST /api/v1/cell

Purpose: update one cell with optimistic concurrency control.

Request body:

```json
{
  "id": "global",
  "index": 513,
  "color": "#111827",
  "if_match_version": 42
}
```

Field rules:

- `id`: required string
- `index`: required integer, range `[0, grid_size * grid_size - 1]`
- `color`: required string, regex `^#[0-9A-Fa-f]{6}$`
- `if_match_version`: required integer `>= 0`

Success response `200`:

```json
{
  "ok": true,
  "version": 43
}
```

Version conflict response `409`:

```json
{
  "ok": false,
  "error": "version_conflict",
  "current_version": 43
}
```

Validation error response `400`:

```json
{
  "ok": false,
  "error": "invalid_request",
  "details": "index out of range"
}
```

Not found response `404`:

```json
{
  "ok": false,
  "error": "canvas_not_found"
}
```

## 6. Update Transaction Requirements

`POST /api/v1/cell` must be atomic.

Pseudo flow:

1. Begin transaction.
2. Lock the canvas row `FOR UPDATE`.
3. Compare `if_match_version` with stored `version`.
4. If mismatch, rollback and return `409`.
5. Update `cells[index]` to normalized uppercase color.
6. Increment `version` by `1`.
7. Set `updated_at = NOW()`.
8. Commit.

Important:

- Never update without version check.
- Never return success without incrementing version.

## 7. JSON and Error Formatting

All responses are JSON.

Recommended headers:

- `Content-Type: application/json; charset=utf-8`

Error body common shape:

```json
{
  "ok": false,
  "error": "error_code",
  "details": "human readable detail"
}
```

## 7.1 Fixed Error Codes

The backend must use the following fixed `error` values:

| HTTP | error              | When to return                                  |
| ---- | ------------------ | ----------------------------------------------- |
| 400  | `invalid_request`  | JSON schema/field validation failed             |
| 404  | `canvas_not_found` | Requested canvas id does not exist              |
| 409  | `version_conflict` | `if_match_version` differs from current version |
| 429  | `rate_limited`     | Request exceeded rate limit policy              |
| 500  | `internal_error`   | Unexpected server error                         |
| 503  | `db_unavailable`   | DB readiness check failed                       |

For 429, response body must include `retry_after_seconds`.

429 response example:

```json
{
  "ok": false,
  "error": "rate_limited",
  "retry_after_seconds": 10
}
```

## 8. Performance and Limits

Recommended limits for first release:

- Request body max size: 16KB

## 8.1 Timeouts and Retry Policy

Server timeout policy:

- Read header timeout: 5s
- Read body timeout: 10s
- Write timeout: 10s
- Idle timeout: 60s
- DB query timeout: 2s (health/ready), 3s (grid read), 5s (cell update)

Frontend retry policy:

- `GET /api/v1/grid`: retry up to 2 times on network errors or 5xx with exponential backoff (`300ms`, `900ms`).
- `POST /api/v1/cell`: do not blind-retry on `409`; immediately re-fetch grid once.
- `POST /api/v1/cell`: retry at most 1 time on network errors only.

## 8.2 Rate Limit Policy

Rate limit unit and scope are fixed as follows:

- Key: client IP
- Window: 10 seconds
- `GET /api/v1/grid`: 60 requests / 10s / IP
- `POST /api/v1/cell`: 30 requests / 10s / IP
- `GET /api/v1/healthz`: unlimited (or very high, no practical cap)
- `GET /api/v1/readyz`: 12 requests / 10s / IP

When blocked, return:

- HTTP `429`
- Body with `error=rate_limited`
- `Retry-After` header in seconds

Expected frontend behavior:

- Poll `GET /api/v1/grid` every 2 seconds
- On `409`, immediately re-fetch grid

## 9. Security Requirements

- CORS allowlist only known frontend origins.
- Validate all request fields strictly.
- Reject malformed colors and out-of-range index.
- Do not expose stack traces in responses.
- Log request id and error code on server side.

## 10. Backward Compatibility Rules

Versioning policy:

- Canonical API namespace is `/api/v1`.
- No breaking changes are allowed inside `/api/v1`.
- Breaking changes require `/api/v2` introduction.

Compatibility policy:

- Current frontend compatibility alias `/api/*` must be preserved while frontend migration is incomplete.
- Alias removal requires: migration PR merged + one release cycle notice.

Field evolution policy:

- Optional response fields may be added.
- Existing required fields must not change type or meaning.
- Request required fields must not be removed in the same API version.

## 11. Example cURL

Get grid:

```bash
curl -s "https://api.keitan1130.com/api/v1/grid?id=global"
```

Update one cell:

```bash
curl -s -X POST "https://api.keitan1130.com/api/v1/cell" \
  -H "Content-Type: application/json" \
  -d '{
    "id":"global",
    "index":513,
    "color":"#111827",
    "if_match_version":42
  }'
```

## 12. Implementation Checklist for Backend AI

- [ ] Create migration for `canvases` table
- [ ] Seed `global` canvas with 32x32 white cells
- [ ] Implement `GET /api/v1/healthz`
- [ ] Implement `GET /api/v1/readyz` with DB check
- [ ] Implement `GET /api/v1/grid`
- [ ] Implement `POST /api/v1/cell` with transaction + version check
- [ ] Expose both `/api/v1/*` and `/api/*` (compatibility alias)
- [ ] Add CORS policy for app domain
- [ ] Add fixed error code mapping and 429 response format
- [ ] Add request timeout and DB timeout settings
- [ ] Add IP-based per-endpoint rate limiting
- [ ] Add strict validation and consistent error responses
- [ ] Add logs and request id propagation

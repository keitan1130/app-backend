package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultPort              = "8080"
	defaultDatabaseURL       = "postgres://pixelgrid:pixelgrid@db:5432/pixelgrid?sslmode=disable"
	defaultAllowedOrigins    = "https://app.keitan1130.com,http://localhost:5173"
	requestBodyMaxBytes int64 = 16 * 1024
)

var colorPattern = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

//go:embed migrations/*.sql
var migrationFS embed.FS

type contextKey string

const requestIDContextKey contextKey = "request_id"

type config struct {
	Port           string
	DatabaseURL    string
	AllowedOrigins map[string]struct{}
}

type application struct {
	db      *pgxpool.Pool
	logger  *slog.Logger
	limiter *rateLimiter
	cfg     config
}

type endpoint struct {
	Method string
	Policy ratePolicy
	Handle func(http.ResponseWriter, *http.Request)
}

type ratePolicy struct {
	Name   string
	Limit  int
	Window time.Duration
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rateBucket
}

type rateBucket struct {
	Count   int
	ResetAt time.Time
}

type statusRecorder struct {
	http.ResponseWriter
	Status int
}

type gridResponse struct {
	ID        string    `json:"id"`
	GridSize  int       `json:"grid_size"`
	Cells     []string  `json:"cells"`
	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

type postCellRequest struct {
	ID             string `json:"id"`
	Index          int    `json:"index"`
	Color          string `json:"color"`
	IfMatchVersion int64  `json:"if_match_version"`
}

type okResponse struct {
	OK bool `json:"ok"`
}

type readyResponse struct {
	OK bool   `json:"ok"`
	DB string `json:"db,omitempty"`
}

type postCellResponse struct {
	OK      bool  `json:"ok"`
	Version int64 `json:"version"`
}

type errorResponse struct {
	OK                bool   `json:"ok"`
	Error             string `json:"error"`
	Details           string `json:"details,omitempty"`
	CurrentVersion    *int64 `json:"current_version,omitempty"`
	RetryAfterSeconds *int   `json:"retry_after_seconds,omitempty"`
}

func (sr *statusRecorder) WriteHeader(statusCode int) {
	sr.Status = statusCode
	sr.ResponseWriter.WriteHeader(statusCode)
}

func main() {
	cfg := loadConfig()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := openDatabaseWithRetry(ctx, cfg.DatabaseURL, logger)
	if err != nil {
		logger.Error("database initialization failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer db.Close()

	app := &application{
		db:      db,
		logger:  logger,
		limiter: newRateLimiter(),
		cfg:     cfg,
	}

	if err := app.runMigrations(ctx); err != nil {
		logger.Error("migration failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		app.logger.Info("server started", slog.String("addr", srv.Addr))
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown failed", slog.String("error", err.Error()))
		}
		logger.Info("server stopped")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}
}

func loadConfig() config {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = defaultPort
	}

	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		databaseURL = defaultDatabaseURL
	}

	allowedOriginsRaw := strings.TrimSpace(os.Getenv("CORS_ALLOWED_ORIGINS"))
	if allowedOriginsRaw == "" {
		allowedOriginsRaw = defaultAllowedOrigins
	}

	allowedOrigins := map[string]struct{}{}
	for _, origin := range strings.Split(allowedOriginsRaw, ",") {
		o := strings.TrimSpace(origin)
		if o != "" {
			allowedOrigins[o] = struct{}{}
		}
	}

	return config{
		Port:           port,
		DatabaseURL:    databaseURL,
		AllowedOrigins: allowedOrigins,
	}
}

func openDatabaseWithRetry(ctx context.Context, databaseURL string, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	poolConfig.MaxConns = 12
	poolConfig.MinConns = 1
	poolConfig.MaxConnLifetime = 30 * time.Minute
	poolConfig.MaxConnIdleTime = 5 * time.Minute

	deadline := time.Now().Add(60 * time.Second)
	for {
		pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
		if err == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			pingErr := pool.Ping(pingCtx)
			cancel()
			if pingErr == nil {
				return pool, nil
			}
			pool.Close()
			err = pingErr
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("database not ready after retries: %w", err)
		}

		logger.Warn("database not ready yet", slog.String("error", err.Error()))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (a *application) runMigrations(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	_, err := a.db.Exec(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version TEXT PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)
`)
	if err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations directory: %w", err)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		version := entry.Name()

		var alreadyApplied bool
		err := a.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&alreadyApplied)
		if err != nil {
			return fmt.Errorf("check migration %s status: %w", version, err)
		}
		if alreadyApplied {
			continue
		}

		sqlBytes, err := migrationFS.ReadFile("migrations/" + version)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", version, err)
		}

		tx, err := a.db.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration tx %s: %w", version, err)
		}

		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("exec migration %s: %w", version, err)
		}

		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("mark migration %s as applied: %w", version, err)
		}

		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", version, err)
		}

		a.logger.Info("migration applied", slog.String("version", version))
	}

	return nil
}

func (a *application) routes() http.Handler {
	window := 10 * time.Second
	policyNone := ratePolicy{}
	policyGrid := ratePolicy{Name: "grid", Limit: 60, Window: window}
	policyCell := ratePolicy{Name: "cell", Limit: 30, Window: window}
	policyReady := ratePolicy{Name: "readyz", Limit: 12, Window: window}

	endpoints := map[string]endpoint{}
	register := func(base string) {
		endpoints[base+"/healthz"] = endpoint{Method: http.MethodGet, Policy: policyNone, Handle: a.healthzHandler}
		endpoints[base+"/readyz"] = endpoint{Method: http.MethodGet, Policy: policyReady, Handle: a.readyzHandler}
		endpoints[base+"/grid"] = endpoint{Method: http.MethodGet, Policy: policyGrid, Handle: a.gridHandler}
		endpoints[base+"/cell"] = endpoint{Method: http.MethodPost, Policy: policyCell, Handle: a.cellHandler}
	}
	register("/api/v1")
	register("/api")

	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ep, ok := endpoints[r.URL.Path]
		if !ok {
			a.respondError(w, r, http.StatusNotFound, "invalid_request", "endpoint not found", nil, nil, nil)
			return
		}

		if r.Method != ep.Method {
			w.Header().Set("Allow", ep.Method+", OPTIONS")
			a.respondError(w, r, http.StatusMethodNotAllowed, "invalid_request", "method not allowed", nil, nil, nil)
			return
		}

		if ep.Policy.Limit > 0 {
			allowed, retryAfterSeconds := a.limiter.Allow(ep.Policy, clientIP(r), time.Now())
			if !allowed {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
				a.respondError(w, r, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded", nil, nil, &retryAfterSeconds)
				return
			}
		}

		ep.Handle(w, r)
	})

	return a.recoverPanic(a.requestID(a.requestLogger(a.cors(dispatch))))
}

func (a *application) healthzHandler(w http.ResponseWriter, r *http.Request) {
	a.respondJSON(w, http.StatusOK, okResponse{OK: true})
}

func (a *application) readyzHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	var one int
	if err := a.db.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		a.respondError(w, r, http.StatusServiceUnavailable, "db_unavailable", "database is unavailable", err, nil, nil)
		return
	}

	a.respondJSON(w, http.StatusOK, readyResponse{OK: true, DB: "up"})
}

func (a *application) gridHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		id = "global"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	var resp gridResponse
	var cellsRaw []byte
	if err := a.db.QueryRow(ctx, `
SELECT id, grid_size, cells, version, updated_at
FROM canvases
WHERE id = $1
`, id).Scan(&resp.ID, &resp.GridSize, &cellsRaw, &resp.Version, &resp.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			a.respondError(w, r, http.StatusNotFound, "canvas_not_found", "canvas not found", nil, nil, nil)
			return
		}
		a.respondError(w, r, http.StatusInternalServerError, "internal_error", "failed to query grid", err, nil, nil)
		return
	}

	if err := json.Unmarshal(cellsRaw, &resp.Cells); err != nil {
		a.respondError(w, r, http.StatusInternalServerError, "internal_error", "failed to parse cells", err, nil, nil)
		return
	}

	normalizedCells, err := validateAndNormalizeCells(resp.Cells, resp.GridSize)
	if err != nil {
		a.respondError(w, r, http.StatusInternalServerError, "internal_error", err.Error(), err, nil, nil)
		return
	}
	resp.Cells = normalizedCells
	resp.UpdatedAt = resp.UpdatedAt.UTC()

	a.respondJSON(w, http.StatusOK, resp)
}

func (a *application) cellHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, requestBodyMaxBytes)
	defer r.Body.Close()

	var req postCellRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		a.respondError(w, r, http.StatusBadRequest, "invalid_request", describeJSONDecodeError(err), nil, nil, nil)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		a.respondError(w, r, http.StatusBadRequest, "invalid_request", "request body must contain a single JSON object", nil, nil, nil)
		return
	}

	if strings.TrimSpace(req.ID) == "" {
		a.respondError(w, r, http.StatusBadRequest, "invalid_request", "id is required", nil, nil, nil)
		return
	}
	if req.IfMatchVersion < 0 {
		a.respondError(w, r, http.StatusBadRequest, "invalid_request", "if_match_version must be >= 0", nil, nil, nil)
		return
	}
	if req.Index < 0 {
		a.respondError(w, r, http.StatusBadRequest, "invalid_request", "index must be >= 0", nil, nil, nil)
		return
	}
	if !colorPattern.MatchString(req.Color) {
		a.respondError(w, r, http.StatusBadRequest, "invalid_request", "color must match #RRGGBB", nil, nil, nil)
		return
	}

	normalizedColor := strings.ToUpper(req.Color)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	tx, err := a.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		a.respondError(w, r, http.StatusInternalServerError, "internal_error", "failed to start transaction", err, nil, nil)
		return
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	var gridSize int
	var version int64
	var cellsRaw []byte
	err = tx.QueryRow(ctx, `
SELECT grid_size, cells, version
FROM canvases
WHERE id = $1
FOR UPDATE
`, req.ID).Scan(&gridSize, &cellsRaw, &version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			a.respondError(w, r, http.StatusNotFound, "canvas_not_found", "canvas not found", nil, nil, nil)
			return
		}
		a.respondError(w, r, http.StatusInternalServerError, "internal_error", "failed to fetch canvas", err, nil, nil)
		return
	}

	if req.IfMatchVersion != version {
		currentVersion := version
		a.respondError(w, r, http.StatusConflict, "version_conflict", "version mismatch", nil, &currentVersion, nil)
		return
	}

	expectedSize := gridSize * gridSize
	if req.Index < 0 || req.Index >= expectedSize {
		a.respondError(w, r, http.StatusBadRequest, "invalid_request", "index out of range", nil, nil, nil)
		return
	}

	var cells []string
	if err := json.Unmarshal(cellsRaw, &cells); err != nil {
		a.respondError(w, r, http.StatusInternalServerError, "internal_error", "failed to parse cells", err, nil, nil)
		return
	}

	normalizedCells, err := validateAndNormalizeCells(cells, gridSize)
	if err != nil {
		a.respondError(w, r, http.StatusInternalServerError, "internal_error", err.Error(), err, nil, nil)
		return
	}

	normalizedCells[req.Index] = normalizedColor
	updatedCellsJSON, err := json.Marshal(normalizedCells)
	if err != nil {
		a.respondError(w, r, http.StatusInternalServerError, "internal_error", "failed to encode cells", err, nil, nil)
		return
	}

	var newVersion int64
	err = tx.QueryRow(ctx, `
UPDATE canvases
SET cells = $1::jsonb,
    version = version + 1,
    updated_at = NOW()
WHERE id = $2 AND version = $3
RETURNING version
`, updatedCellsJSON, req.ID, version).Scan(&newVersion)
	if err != nil {
		a.respondError(w, r, http.StatusInternalServerError, "internal_error", "failed to update canvas", err, nil, nil)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		a.respondError(w, r, http.StatusInternalServerError, "internal_error", "failed to commit transaction", err, nil, nil)
		return
	}

	a.respondJSON(w, http.StatusOK, postCellResponse{OK: true, Version: newVersion})
}

func describeJSONDecodeError(err error) string {
	var syntaxError *json.SyntaxError
	var unmarshalTypeError *json.UnmarshalTypeError
	if errors.As(err, &syntaxError) {
		return fmt.Sprintf("malformed JSON at position %d", syntaxError.Offset)
	}
	if errors.As(err, &unmarshalTypeError) {
		return fmt.Sprintf("invalid value for field %s", unmarshalTypeError.Field)
	}
	if errors.Is(err, io.EOF) {
		return "request body is required"
	}
	if strings.Contains(err.Error(), "http: request body too large") {
		return "request body too large (max 16KB)"
	}
	return "invalid JSON body"
}

func validateAndNormalizeCells(cells []string, gridSize int) ([]string, error) {
	expected := gridSize * gridSize
	if len(cells) != expected {
		return nil, fmt.Errorf("cells length mismatch")
	}

	normalized := make([]string, len(cells))
	for i, cell := range cells {
		if !colorPattern.MatchString(cell) {
			return nil, fmt.Errorf("invalid color at index %d", i)
		}
		normalized[i] = strings.ToUpper(cell)
	}
	return normalized, nil
}

func (a *application) respondJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (a *application) respondError(w http.ResponseWriter, r *http.Request, status int, code, details string, err error, currentVersion *int64, retryAfterSeconds *int) {
	payload := errorResponse{
		OK:      false,
		Error:   code,
		Details: details,
	}
	if currentVersion != nil {
		payload.CurrentVersion = currentVersion
	}
	if retryAfterSeconds != nil {
		payload.RetryAfterSeconds = retryAfterSeconds
	}

	a.respondJSON(w, status, payload)
	a.logError(r, status, code, details, err)
}

func (a *application) logError(r *http.Request, status int, code, details string, err error) {
	args := []any{
		slog.String("request_id", requestIDFromContext(r.Context())),
		slog.String("error_code", code),
		slog.Int("status", status),
		slog.String("path", r.URL.Path),
		slog.String("method", r.Method),
		slog.String("details", details),
	}
	if err != nil {
		args = append(args, slog.String("error", err.Error()))
	}
	a.logger.Error("request failed", args...)
}

func (a *application) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if requestID == "" {
			requestID = generateRequestID()
		}
		w.Header().Set("X-Request-Id", requestID)
		ctx := context.WithValue(r.Context(), requestIDContextKey, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *application) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, Status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		a.logger.Info("request completed",
			slog.String("request_id", requestIDFromContext(r.Context())),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", recorder.Status),
			slog.String("ip", clientIP(r)),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

func (a *application) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != "" {
			w.Header().Add("Vary", "Origin")
			if _, ok := a.cfg.AllowedOrigins[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			}
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (a *application) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				a.respondError(w, r, http.StatusInternalServerError, "internal_error", "unexpected server error", fmt.Errorf("panic: %v", recovered), nil, nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	if cfIP := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cfIP != "" {
		return cfIP
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func generateRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		now := time.Now().UnixNano()
		for i := 0; i < len(buf); i++ {
			buf[i] = byte(now >> (uint(i%8) * 8))
		}
	}
	return hex.EncodeToString(buf)
}

func requestIDFromContext(ctx context.Context) string {
	v, ok := ctx.Value(requestIDContextKey).(string)
	if !ok {
		return ""
	}
	return v
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{buckets: map[string]*rateBucket{}}
}

func (rl *rateLimiter) Allow(policy ratePolicy, clientKey string, now time.Time) (bool, int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if policy.Limit <= 0 {
		return true, 0
	}

	bucketKey := policy.Name + ":" + clientKey
	bucket, ok := rl.buckets[bucketKey]
	if !ok || now.After(bucket.ResetAt) {
		rl.buckets[bucketKey] = &rateBucket{Count: 1, ResetAt: now.Add(policy.Window)}
		rl.gc(now)
		return true, 0
	}

	if bucket.Count >= policy.Limit {
		retry := int(bucket.ResetAt.Sub(now).Seconds())
		if retry < 1 {
			retry = 1
		}
		return false, retry
	}

	bucket.Count++
	return true, 0
}

func (rl *rateLimiter) gc(now time.Time) {
	if len(rl.buckets) < 2048 {
		return
	}
	for key, bucket := range rl.buckets {
		if now.After(bucket.ResetAt.Add(15 * time.Second)) {
			delete(rl.buckets, key)
		}
	}
}

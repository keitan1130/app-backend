package main

import (
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultPort                       = "8080"
	defaultDatabaseURL                = "postgres://pixelgrid:pixelgrid@db:5432/pixelgrid?sslmode=disable"
	defaultAllowedOrigins             = "https://app.keitan1130.com,http://localhost:5173"
	defaultTrustedProxyCIDRs          = ""
	defaultMarkItDownTimeoutSec       = 30
	requestBodyMaxBytes         int64 = 16 * 1024
	markItDownMaxBytes          int64 = 25 * 1024 * 1024
	markItDownWriteTimeoutPad         = 10 * time.Second
)

var colorPattern = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

type contextKey string

const requestIDContextKey contextKey = "request_id"

type config struct {
	Port                   string
	DatabaseURL            string
	AllowedOrigins         map[string]struct{}
	TrustedProxyCIDRs      []*net.IPNet
	MarkItDownTimeout      time.Duration
	MarkItDownWriteTimeout time.Duration
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

type markItDownResponse struct {
	OK       bool   `json:"ok"`
	Filename string `json:"filename"`
	Markdown string `json:"markdown"`
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

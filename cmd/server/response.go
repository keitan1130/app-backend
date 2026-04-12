package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

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

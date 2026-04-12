package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

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

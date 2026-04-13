package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type markItDownInputRequest struct {
	Input string `json:"input"`
}

var allowedMarkItDownFileExtensions = map[string]struct{}{
	".pdf":  {},
	".ppt":  {},
	".pptx": {},
	".doc":  {},
	".docx": {},
	".xls":  {},
	".xlsx": {},
	".html": {},
	".htm":  {},
	".csv":  {},
	".json": {},
	".xml":  {},
}

func (a *application) markItDownHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, markItDownMaxBytes)
	defer r.Body.Close()
	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))

	if strings.HasPrefix(contentType, "application/json") {
		sourceArg, filename, cleanup, err := parseMarkItDownJSONInput(r)
		if err != nil {
			a.respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error(), nil, nil, nil)
			return
		}
		if cleanup != nil {
			defer cleanup()
		}

		convertCtx, cancel := context.WithTimeout(r.Context(), a.cfg.MarkItDownTimeout)
		defer cancel()

		markdown, err := runMarkItDown(convertCtx, sourceArg)
		if err != nil {
			a.respondError(w, r, http.StatusInternalServerError, "internal_error", "failed to convert input with markitdown", err, nil, nil)
			return
		}

		markdown = applyImageEmptyResultFallback(markdown, filename)

		a.respondJSON(w, http.StatusOK, markItDownResponse{OK: true, Filename: filename, Markdown: markdown})
		return
	}

	if contentType != "" && !strings.HasPrefix(contentType, "multipart/form-data") {
		a.respondError(w, r, http.StatusBadRequest, "invalid_request", "Content-Type must be multipart/form-data or application/json", nil, nil, nil)
		return
	}

	uploadFile, fileHeader, err := r.FormFile("file")
	if err != nil {
		if strings.Contains(err.Error(), "http: request body too large") {
			a.respondError(w, r, http.StatusRequestEntityTooLarge, "invalid_request", "request body too large (max 25MB)", nil, nil, nil)
			return
		}
		if errors.Is(err, http.ErrMissingFile) {
			a.respondError(w, r, http.StatusBadRequest, "invalid_request", "multipart/form-data field 'file' is required", nil, nil, nil)
			return
		}
		a.respondError(w, r, http.StatusBadRequest, "invalid_request", "multipart/form-data request is required", err, nil, nil)
		return
	}
	defer uploadFile.Close()

	filename := filepath.Base(strings.TrimSpace(fileHeader.Filename))
	if filename == "" {
		filename = "document"
	}

	ext := filepath.Ext(filename)
	if len(ext) > 16 {
		ext = ""
	}

	if !isAllowedMarkItDownExtension(ext) {
		a.respondError(w, r, http.StatusBadRequest, "invalid_request", "unsupported file type: only PDF, PowerPoint, Word, Excel, HTML, CSV, JSON, XML are allowed", nil, nil, nil)
		return
	}

	tempFile, err := os.CreateTemp("", "markitdown-*"+ext)
	if err != nil {
		a.respondError(w, r, http.StatusInternalServerError, "internal_error", "failed to create temporary file", err, nil, nil)
		return
	}
	tempFilePath := tempFile.Name()
	defer func() {
		_ = os.Remove(tempFilePath)
	}()

	if _, err := io.Copy(tempFile, uploadFile); err != nil {
		_ = tempFile.Close()
		a.respondError(w, r, http.StatusInternalServerError, "internal_error", "failed to persist uploaded file", err, nil, nil)
		return
	}

	if err := tempFile.Close(); err != nil {
		a.respondError(w, r, http.StatusInternalServerError, "internal_error", "failed to finalize uploaded file", err, nil, nil)
		return
	}

	convertCtx, cancel := context.WithTimeout(r.Context(), a.cfg.MarkItDownTimeout)
	defer cancel()

	markdown, err := runMarkItDown(convertCtx, tempFilePath)
	if err != nil {
		a.respondError(w, r, http.StatusInternalServerError, "internal_error", "failed to convert file with markitdown", err, nil, nil)
		return
	}

	markdown = applyImageEmptyResultFallback(markdown, filename)

	a.respondJSON(w, http.StatusOK, markItDownResponse{OK: true, Filename: filename, Markdown: markdown})
}

func parseMarkItDownJSONInput(r *http.Request) (sourceArg string, filename string, cleanup func(), err error) {
	var req markItDownInputRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return "", "", nil, errors.New("application/json body with 'input' is required")
	}

	input := strings.TrimSpace(req.Input)
	if input == "" {
		return "", "", nil, errors.New("field 'input' must not be empty")
	}

	if isHTTPURL(input) {
		return "", "", nil, errors.New("URL input is not supported: only HTML, CSV, JSON, XML text is allowed")
	}

	manualExt := detectManualTextExtension(input)
	if manualExt == "" {
		return "", "", nil, errors.New("manual input must be HTML, CSV, JSON, or XML text")
	}

	tempFile, err := os.CreateTemp("", "markitdown-input-*"+manualExt)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to create temporary input file: %w", err)
	}

	tempFilePath := tempFile.Name()
	if _, err := tempFile.WriteString(input); err != nil {
		_ = tempFile.Close()
		_ = os.Remove(tempFilePath)
		return "", "", nil, fmt.Errorf("failed to write temporary input file: %w", err)
	}

	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempFilePath)
		return "", "", nil, fmt.Errorf("failed to finalize temporary input file: %w", err)
	}

	return tempFilePath, "manual-input" + manualExt, func() {
		_ = os.Remove(tempFilePath)
	}, nil
}

func detectManualTextExtension(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}

	if json.Valid([]byte(trimmed)) {
		return ".json"
	}

	if looksLikeHTML(trimmed) {
		return ".html"
	}

	if looksLikeXML(trimmed) {
		return ".xml"
	}

	if looksLikeCSV(trimmed) {
		return ".csv"
	}

	return ""
}

func looksLikeXML(input string) bool {
	if !strings.HasPrefix(input, "<") || !strings.HasSuffix(input, ">") {
		return false
	}

	decoder := xml.NewDecoder(strings.NewReader(input))
	for {
		_, err := decoder.Token()
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return true
		}
		return false
	}
}

func looksLikeHTML(input string) bool {
	lower := strings.ToLower(strings.TrimSpace(input))
	if strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") {
		return true
	}

	if strings.Contains(lower, "</html>") || strings.Contains(lower, "<body") || strings.Contains(lower, "<head") {
		return true
	}

	return false
}

func looksLikeCSV(input string) bool {
	if !strings.Contains(input, ",") {
		return false
	}

	lines := strings.Split(input, "\n")
	nonEmpty := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		nonEmpty++
		if nonEmpty >= 2 {
			return true
		}
	}

	return false
}

func isAllowedMarkItDownExtension(ext string) bool {
	normalized := strings.ToLower(strings.TrimSpace(ext))
	_, ok := allowedMarkItDownFileExtensions[normalized]
	return ok
}

func isHTTPURL(input string) bool {
	parsed, err := url.Parse(strings.TrimSpace(input))
	if err != nil {
		return false
	}
	if parsed == nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return parsed.Hostname() != ""
}

func runMarkItDown(ctx context.Context, filePath string) (string, error) {
	commands := [][]string{
		{"markitdown", filePath},
		{"python3", "-m", "markitdown", filePath},
		{"python", "-m", "markitdown", filePath},
	}

	var lastErr error
	for _, parts := range commands {
		var stdout bytes.Buffer
		var stderr bytes.Buffer

		cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		if err == nil {
			return stdout.String(), nil
		}

		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("markitdown timeout or cancellation: %w", ctxErr)
		}

		if errors.Is(err, exec.ErrNotFound) {
			lastErr = fmt.Errorf("command not found: %s", parts[0])
			continue
		}

		stderrText := strings.TrimSpace(stderr.String())
		if stderrText != "" {
			lastErr = fmt.Errorf("command %s failed: %s", parts[0], stderrText)
			continue
		}

		lastErr = err
	}

	if lastErr == nil {
		lastErr = errors.New("markitdown command unavailable")
	}

	return "", lastErr
}

func applyImageEmptyResultFallback(markdown string, filename string) string {
	if strings.TrimSpace(markdown) != "" {
		return markdown
	}

	if !isLikelyImageFilename(filename) {
		return markdown
	}

	return strings.Join([]string{
		"No extractable text was returned for this image.",
		"",
		"MarkItDown CLI may return empty output for images when EXIF metadata is unavailable and no OCR/caption backend is configured.",
		"",
		"Try one of the following:",
		"- Install exiftool to extract image metadata.",
		"- Use MarkItDown Python API with llm_client + llm_model for image descriptions.",
		"- Use Azure Document Intelligence mode for OCR.",
	}, "\n")
}

func isLikelyImageFilename(filename string) bool {
	ext := strings.ToLower(strings.TrimSpace(filepath.Ext(filename)))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".bmp", ".tif", ".tiff", ".webp", ".gif":
		return true
	default:
		return false
	}
}

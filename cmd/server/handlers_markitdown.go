package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

func (a *application) markItDownHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, markItDownMaxBytes)
	defer r.Body.Close()
	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))

	if strings.HasPrefix(contentType, "application/json") {
		sourceArg, filename, cleanup, err := parseMarkItDownJSONInput(r, a.cfg.MarkItDownDomains)
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

	a.respondJSON(w, http.StatusOK, markItDownResponse{OK: true, Filename: filename, Markdown: markdown})
}

func parseMarkItDownJSONInput(r *http.Request, allowedDomains map[string]struct{}) (sourceArg string, filename string, cleanup func(), err error) {
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
		if err := validateMarkItDownURL(r.Context(), input, allowedDomains); err != nil {
			return "", "", nil, err
		}
		return input, deriveFilenameFromURL(input), nil, nil
	}

	tempFile, err := os.CreateTemp("", "markitdown-input-*.txt")
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

	return tempFilePath, "manual-input.txt", func() {
		_ = os.Remove(tempFilePath)
	}, nil
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

func validateMarkItDownURL(ctx context.Context, rawURL string, allowedDomains map[string]struct{}) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed == nil {
		return errors.New("invalid URL")
	}

	host := normalizeDomain(parsed.Hostname())
	if host == "" {
		return errors.New("URL host is required")
	}

	if host == "localhost" {
		return errors.New("localhost URL is not allowed")
	}

	if !isAllowedDomain(host, allowedDomains) {
		return errors.New("URL domain is not allowed")
	}

	if parsedIP := net.ParseIP(host); parsedIP != nil {
		if isBlockedTargetIP(parsedIP) {
			return errors.New("URL resolves to a disallowed network")
		}
		return nil
	}

	resolveCtx, cancel := context.WithTimeout(ctx, markItDownDNSResolveTimeout)
	defer cancel()

	ips, err := net.DefaultResolver.LookupIPAddr(resolveCtx, host)
	if err != nil || len(ips) == 0 {
		return errors.New("failed to resolve URL hostname")
	}

	for _, ipAddr := range ips {
		if isBlockedTargetIP(ipAddr.IP) {
			return errors.New("URL resolves to a disallowed network")
		}
	}

	return nil
}

func isAllowedDomain(host string, allowedDomains map[string]struct{}) bool {
	if len(allowedDomains) == 0 {
		return false
	}

	for domain := range allowedDomains {
		if domain == "" {
			continue
		}
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}

	return false
}

func normalizeDomain(input string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(input)), ".")
}

func isBlockedTargetIP(ip net.IP) bool {
	if ip == nil {
		return true
	}

	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}

	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
		if v4[0] == 198 && (v4[1] == 18 || v4[1] == 19) {
			return true
		}
	}

	return false
}

func deriveFilenameFromURL(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "source-url"
	}

	base := filepath.Base(strings.TrimSpace(parsed.Path))
	if base == "" || base == "." || base == "/" {
		host := strings.TrimSpace(parsed.Hostname())
		if host == "" {
			return "source-url"
		}
		return sanitizeFilename(host)
	}

	return sanitizeFilename(base)
}

func sanitizeFilename(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return "source"
	}

	b := strings.Builder{}
	b.Grow(len(input))
	for _, r := range input {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	out := strings.Trim(b.String(), "-._")
	if out == "" {
		return "source"
	}

	return out
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

package main

import (
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

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

	trustedProxyCIDRs := parseTrustedProxyCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS"))
	domains := parseMarkItDownDomains(os.Getenv("MARKITDOWN_ALLOWED_DOMAINS"))
	markItDownTimeout := parseMarkItDownTimeout(os.Getenv("MARKITDOWN_TIMEOUT_SECONDS"))

	return config{
		Port:                   port,
		DatabaseURL:            databaseURL,
		AllowedOrigins:         allowedOrigins,
		TrustedProxyCIDRs:      trustedProxyCIDRs,
		MarkItDownDomains:      domains,
		MarkItDownTimeout:      markItDownTimeout,
		MarkItDownWriteTimeout: markItDownTimeout + markItDownWriteTimeoutPad,
	}
}

func parseTrustedProxyCIDRs(raw string) []*net.IPNet {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultTrustedProxyCIDRs
	}

	trusted := []*net.IPNet{}
	for _, part := range strings.Split(raw, ",") {
		cidr := strings.TrimSpace(part)
		if cidr == "" {
			continue
		}
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		trusted = append(trusted, network)
	}

	return trusted
}

func parseMarkItDownDomains(raw string) map[string]struct{} {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultMarkItDownDomains
	}

	domains := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		domain := normalizeDomain(part)
		if domain == "" {
			continue
		}
		domains[domain] = struct{}{}
	}

	return domains
}

func parseMarkItDownTimeout(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Duration(defaultMarkItDownTimeoutSec) * time.Second
	}

	sec, err := strconv.Atoi(raw)
	if err != nil {
		return time.Duration(defaultMarkItDownTimeoutSec) * time.Second
	}

	if sec < 5 {
		sec = 5
	}
	if sec > 180 {
		sec = 180
	}

	return time.Duration(sec) * time.Second
}

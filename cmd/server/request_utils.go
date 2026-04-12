package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"time"
)

func clientIP(r *http.Request, trustedProxyCIDRs []*net.IPNet) string {
	remote := parseRemoteIP(r.RemoteAddr)
	if remote == nil {
		return strings.TrimSpace(r.RemoteAddr)
	}

	if !isTrustedProxy(remote, trustedProxyCIDRs) {
		return remote.String()
	}

	if cfIP := parseIP(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); cfIP != nil {
		return cfIP.String()
	}

	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			if forwarded := parseIP(strings.TrimSpace(parts[0])); forwarded != nil {
				return forwarded.String()
			}
		}
	}

	return remote.String()
}

func parseRemoteIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err == nil && host != "" {
		return parseIP(host)
	}
	return parseIP(strings.TrimSpace(remoteAddr))
}

func parseIP(value string) net.IP {
	ip := net.ParseIP(strings.TrimSpace(value))
	if ip == nil {
		return nil
	}
	return ip
}

func isTrustedProxy(ip net.IP, trustedProxyCIDRs []*net.IPNet) bool {
	for _, network := range trustedProxyCIDRs {
		if network != nil && network.Contains(ip) {
			return true
		}
	}
	return false
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

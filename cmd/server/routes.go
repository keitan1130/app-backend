package main

import (
	"net/http"
	"strconv"
	"time"
)

func (a *application) routes() http.Handler {
	window := 10 * time.Second
	policyNone := ratePolicy{}
	policyGrid := ratePolicy{Name: "grid", Limit: 60, Window: window}
	policyCell := ratePolicy{Name: "cell", Limit: 30, Window: window}
	policyReady := ratePolicy{Name: "readyz", Limit: 12, Window: window}
	policyMarkItDown := ratePolicy{Name: "markitdown", Limit: 8, Window: window}

	endpoints := map[string]endpoint{}
	register := func(base string) {
		endpoints[base+"/healthz"] = endpoint{Method: http.MethodGet, Policy: policyNone, Handle: a.healthzHandler}
		endpoints[base+"/readyz"] = endpoint{Method: http.MethodGet, Policy: policyReady, Handle: a.readyzHandler}
		endpoints[base+"/grid"] = endpoint{Method: http.MethodGet, Policy: policyGrid, Handle: a.gridHandler}
		endpoints[base+"/cell"] = endpoint{Method: http.MethodPost, Policy: policyCell, Handle: a.cellHandler}
		endpoints[base+"/markitdown"] = endpoint{Method: http.MethodPost, Policy: policyMarkItDown, Handle: a.markItDownHandler}
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
			allowed, retryAfterSeconds := a.limiter.Allow(ep.Policy, clientIP(r, a.cfg.TrustedProxyCIDRs), time.Now())
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

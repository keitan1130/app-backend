package main

import "time"

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

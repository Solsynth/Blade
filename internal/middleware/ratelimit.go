package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

type RateLimiter struct {
	limiters map[string]*clientLimiter
	mu       sync.RWMutex
	rate     rate.Limit
	burst    int
}

type clientLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func NewRateLimiter(requestsPerMinute int, burst int) *RateLimiter {
	rl := &RateLimiter{
		limiters: make(map[string]*clientLimiter),
		rate:     rate.Limit(requestsPerMinute) / 60,
		burst:    burst,
	}

	go rl.cleanup()

	return rl
}

func (rl *RateLimiter) getClientLimiter(ip string) *clientLimiter {
	rl.mu.RLock()
	limiter, exists := rl.limiters[ip]
	rl.mu.RUnlock()

	if exists {
		return limiter
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	if limiter, exists = rl.limiters[ip]; exists {
		return limiter
	}

	limiter = &clientLimiter{
		limiter:  rate.NewLimiter(rl.rate, rl.burst),
		lastSeen: time.Now(),
	}
	rl.limiters[ip] = limiter
	return limiter
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		rl.mu.Lock()
		for ip, cl := range rl.limiters {
			if time.Since(cl.lastSeen) > 3*time.Minute {
				delete(rl.limiters, ip)
			}
		}
		rl.mu.Unlock()
	}
}

func (rl *RateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()

		limiter := rl.getClientLimiter(ip)

		if !limiter.limiter.Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "too many requests",
				"code":  "RATE_LIMIT_EXCEEDED",
			})
			return
		}

		rl.mu.Lock()
		if cl, exists := rl.limiters[ip]; exists {
			cl.lastSeen = time.Now()
		}
		rl.mu.Unlock()

		c.Next()
	}
}

package safety

import (
	"context"
	"net/url"
	"sync"
	"time"
)

// Limiter enforces politeness towards a single site: how many pages may be
// rendered at once, and how often.
//
// This is not rate limiting for our own benefit. A distiller that opens eight
// headless tabs against a small studio's site and hammers it is indistinguishable
// from an attack, and the crawl-delay a site publishes is a request that should
// be honoured rather than logged.
type Limiter struct {
	concurrency int
	minInterval time.Duration

	mu    sync.Mutex
	hosts map[string]*hostLimiter
}

type hostLimiter struct {
	slots chan struct{}
	mu    sync.Mutex
	next  time.Time
	delay time.Duration
}

// NewLimiter builds a limiter. concurrency is per host, not global.
func NewLimiter(concurrency int, minInterval time.Duration) *Limiter {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Limiter{
		concurrency: concurrency,
		minInterval: minInterval,
		hosts:       map[string]*hostLimiter{},
	}
}

// SetDelay records a crawl-delay for a host, as published in its robots.txt.
// The larger of the published delay and the configured minimum wins: a site
// asking to be crawled more slowly is obeyed, a site asking to be crawled
// faster than our own floor is not.
func (l *Limiter) SetDelay(host string, d time.Duration) {
	h := l.forHost(host)
	h.mu.Lock()
	if d > h.delay {
		h.delay = d
	}
	h.mu.Unlock()
}

// Acquire blocks until it is polite to fetch from this host, and returns the
// function that releases the slot.
func (l *Limiter) Acquire(ctx context.Context, u *url.URL) (release func(), err error) {
	h := l.forHost(u.Hostname())

	select {
	case h.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	h.mu.Lock()
	delay := h.delay
	if delay < l.minInterval {
		delay = l.minInterval
	}
	wait := time.Until(h.next)
	h.next = time.Now().Add(maxDur(wait, 0) + delay)
	h.mu.Unlock()

	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			<-h.slots
			return nil, ctx.Err()
		}
	}

	var once sync.Once
	return func() { once.Do(func() { <-h.slots }) }, nil
}

func (l *Limiter) forHost(host string) *hostLimiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	h, ok := l.hosts[host]
	if !ok {
		h = &hostLimiter{slots: make(chan struct{}, l.concurrency)}
		l.hosts[host] = h
	}
	return h
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

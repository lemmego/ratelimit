// Package ratelimit refuses a request that has been made too often.
//
// It is a fixed-window counter over the application's cache, which means it
// is shared across processes when the cache is Redis and per-process when the
// cache is in memory. That distinction matters: an in-memory limiter behind
// three instances allows three times what it says.
//
//	r.Post("/login", ratelimit.Middleware(ratelimit.PerIP(5, time.Minute)), handlers.Login)
//
//	if ok, retryAfter, err := ratelimit.Allow(ctx, "vote:"+userID, 30, time.Minute); !ok {
//	    …
//	}
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/lemmego/api/app"
	"github.com/lemmego/auth"
	"github.com/lemmego/cache"
)

// KeyPrefix namespaces every counter, so flushing the limiter cannot flush
// anything else and a stray key cannot be mistaken for application data.
const KeyPrefix = "ratelimit:"

// Allow records one hit against key and reports whether it is within limit
// for the current window.
//
// When it refuses, the second return is how long until the window rolls, so a
// handler can set Retry-After. That is not a courtesy: without it a client
// that retries immediately turns one refused request into a tight loop, and
// the limiter becomes the load it was meant to prevent.
//
// A cache failure allows the request. A limiter is a safety belt, and a
// broken safety belt must not stop the car — refusing everything because
// Redis is briefly unreachable converts a cache outage into a total outage.
// The failure is logged, at warning, because silently ceasing to rate-limit
// is its own kind of incident.
func Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	if limit <= 0 {
		return false, window, nil
	}
	if window <= 0 {
		return true, 0, nil
	}

	// The window is identified by the slot it falls in, so every key in a
	// given window expires together and a counter cannot drift forward one
	// hit at a time. It also makes the remaining time computable without
	// storing a timestamp.
	slot := time.Now().UnixNano() / int64(window)
	windowKey := KeyPrefix + key + ":" + strconv.FormatInt(slot, 36)
	remaining := time.Duration((slot+1)*int64(window) - time.Now().UnixNano())

	// Seed the counter with a TTL before incrementing. cache.Increment takes
	// no TTL, so a key created by Increment alone would have the store's
	// default — or none — and the window would never roll.
	if _, err := cache.Add(ctx, windowKey, 0, window); err != nil {
		slog.Warn("ratelimit: the cache is unavailable, so this request is not being limited",
			"key", key, "error", err)
		return true, 0, err
	}

	count, err := cache.Increment(ctx, windowKey, 1)
	if err != nil {
		slog.Warn("ratelimit: the cache is unavailable, so this request is not being limited",
			"key", key, "error", err)
		return true, 0, err
	}

	if count > int64(limit) {
		return false, remaining, nil
	}
	return true, remaining, nil
}

// Opts configures the middleware.
type Opts struct {
	// Limit is how many requests one key may make per window.
	Limit int
	// Window is how long the window lasts.
	Window time.Duration
	// Key identifies the caller. Defaults to the client IP.
	Key func(c app.Context) string
	// Message is shown when the limit is exceeded.
	Message string
}

// PerIP limits one client IP's use of one route. Use it for anything an
// anonymous visitor can reach: login, registration, password reset.
func PerIP(limit int, window time.Duration) *Opts {
	return &Opts{Limit: limit, Window: window, Key: func(c app.Context) string {
		return "ip:" + ClientIP(c) + "|" + Route(c)
	}}
}

// PerUser limits one authenticated user's use of one route, falling back to
// the IP for an anonymous request.
//
// Keying on the user is what you want for an action only a signed-in person
// can take — voting, commenting, posting — because an IP is shared by an
// office, a university and a mobile carrier's NAT, and limiting those as one
// caller punishes the wrong people.
//
// The matched route is part of the key, so each limit counts its own action.
// Two routes limited at 20 an hour give a caller 20 each rather than 20
// between them, and a limit's threshold cannot be spent by traffic to a
// different route.
//
// To share one budget across several routes, set Opts.Key to something that
// does not vary by route.
func PerUser(limit int, window time.Duration) *Opts {
	return &Opts{Limit: limit, Window: window, Key: func(c app.Context) string {
		return Caller(c) + "|" + Route(c)
	}}
}

// PerRoute is an alias for PerUser, kept because the name reads well at a
// call site.
func PerRoute(limit int, window time.Duration) *Opts {
	return PerUser(limit, window)
}

// Caller identifies who is asking: the authenticated user, or the client IP
// when nobody is signed in.
func Caller(c app.Context) string {
	if id, ok := auth.UserID(c); ok && id != "" {
		return "user:" + id
	}
	return "ip:" + ClientIP(c)
}

// Route identifies what is being asked for.
//
// It is the *matched pattern* rather than the request path, which matters for
// any route carrying a parameter: keying on the path would give
// /launch/a/upvote and /launch/b/upvote separate counters, turning "20 upvotes
// an hour" into "20 an hour per launch" — a limit that counts nothing an
// abuser cannot step around by picking a different target.
//
// http.ServeMux records the pattern on the request, so this costs nothing and
// needs no router dependency. The path is the fallback for a handler reached
// without going through the mux, such as one mounted directly or called from
// a test.
func Route(c app.Context) string {
	request := c.Request()
	if pattern := request.Pattern; pattern != "" {
		return pattern
	}
	return request.Method + " " + request.URL.Path
}

// ClientIP returns the caller's address.
//
// It reads X-Forwarded-For only when TrustProxy has been enabled, because
// that header is client-supplied: trusting it unconditionally lets anyone
// bypass an IP limit by sending a different value on every request, which is
// worse than having no limiter at all.
func ClientIP(c app.Context) string {
	if trustProxy {
		if forwarded := c.Header("X-Forwarded-For"); forwarded != "" {
			// The left-most entry is the original client; the rest were added
			// by each hop.
			if idx := indexByte(forwarded, ','); idx > 0 {
				return trimSpace(forwarded[:idx])
			}
			return trimSpace(forwarded)
		}
		if real := c.Header("X-Real-IP"); real != "" {
			return trimSpace(real)
		}
	}
	host, _, err := net.SplitHostPort(c.Request().RemoteAddr)
	if err != nil {
		return c.Request().RemoteAddr
	}
	return host
}

// trustProxy gates the forwarded headers. It is a package-level switch set at
// boot rather than a per-call option, because getting it wrong in one place
// is enough to make every IP limit bypassable.
var trustProxy bool

// TrustProxy tells ClientIP to believe X-Forwarded-For and X-Real-IP. Enable
// it only when the application is behind a proxy that overwrites those
// headers — which your own load balancer does and the open internet does not.
func TrustProxy(trust bool) { trustProxy = trust }

// Middleware refuses a request over the limit with 429 and a Retry-After
// header.
func Middleware(opts *Opts) app.Handler {
	if opts == nil {
		panic("ratelimit: Middleware needs options")
	}
	key := opts.Key
	if key == nil {
		key = ClientIP
	}
	message := opts.Message
	if message == "" {
		message = "too many requests"
	}

	return func(c app.Context) error {
		allowed, retryAfter, _ := Allow(c.RequestContext(), key(c), opts.Limit, opts.Window)
		if allowed {
			return c.Next()
		}

		seconds := int(retryAfter.Seconds())
		if seconds < 1 {
			seconds = 1
		}
		c.SetHeader("Retry-After", strconv.Itoa(seconds))
		return c.Error(http.StatusTooManyRequests,
			fmt.Errorf("%s: try again in %ds", message, seconds))
	}
}

// ErrNoCache means the cache is not configured, so nothing can be limited.
var ErrNoCache = errors.New("ratelimit: no cache is configured")

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

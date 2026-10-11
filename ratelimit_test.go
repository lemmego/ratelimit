package ratelimit

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/lemmego/api/app"
	"github.com/lemmego/api/config"
	"github.com/lemmego/apptest"
	"github.com/lemmego/cache"
	_ "github.com/lemmego/cache/store/memory"
)

// The provider is configured explicitly rather than through a config map, so
// the test does not depend on the shape of a project's cache config file.
func cacheProvider() app.Provider {
	return &cache.Provider{Config: &cache.Config{
		Driver: "memory",
		Prefix: "ratelimit_test:",
		Codec:  "json",
	}}
}

func cacheConfig() config.M {
	return config.M{"app": config.M{"name": "ratelimit", "env": "test", "port": 0}}
}

func boot(t *testing.T, opts *Opts) *apptest.App {
	t.Helper()
	return apptest.New(t,
		apptest.WithConfig(cacheConfig()),
		apptest.WithProviders(cacheProvider()),
		apptest.WithRoutes(func(a app.App) {
			a.Router().Get("/limited", Middleware(opts), func(c app.Context) error {
				return c.JSON(app.M{"ok": true})
			})
		}),
	)
}

// The counter has to persist across requests, which is the whole job.
func TestTheLimitIsEnforcedAcrossRequests(t *testing.T) {
	a := boot(t, &Opts{Limit: 3, Window: time.Minute, Key: func(app.Context) string {
		return "fixed-key-" + t.Name()
	}})

	for i := 1; i <= 3; i++ {
		a.Get("/limited").Send().AssertStatus(http.StatusOK)
	}
	a.Get("/limited").Send().AssertStatus(http.StatusTooManyRequests)
}

// Retry-After is not a courtesy. Without it a client that retries
// immediately turns one refusal into a tight loop, and the limiter becomes
// the load it was meant to prevent.
func TestARefusalSaysWhenToRetry(t *testing.T) {
	a := boot(t, &Opts{Limit: 1, Window: 90 * time.Second, Key: func(app.Context) string {
		return "retry-after-" + t.Name()
	}})

	a.Get("/limited").Send().AssertStatus(http.StatusOK)
	res := a.Get("/limited").Send().AssertStatus(http.StatusTooManyRequests)

	header := res.Header.Get("Retry-After")
	seconds, err := strconv.Atoi(header)
	if err != nil {
		t.Fatalf("Retry-After = %q, want an integer", header)
	}
	if seconds < 1 || seconds > 90 {
		t.Errorf("Retry-After = %d, want between 1 and the window", seconds)
	}
}

// Two callers must not share a budget.
func TestCallersAreLimitedSeparately(t *testing.T) {
	var caller string
	a := apptest.New(t,
		apptest.WithConfig(cacheConfig()),
		apptest.WithProviders(cacheProvider()),
		apptest.WithRoutes(func(a app.App) {
			a.Router().Get("/limited",
				Middleware(&Opts{Limit: 1, Window: time.Minute, Key: func(app.Context) string {
					return "separate-" + t.Name() + "-" + caller
				}}),
				func(c app.Context) error { return c.JSON(app.M{"ok": true}) })
		}),
	)

	caller = "ada"
	a.Get("/limited").Send().AssertStatus(http.StatusOK)
	a.Get("/limited").Send().AssertStatus(http.StatusTooManyRequests)

	// A different caller starts fresh.
	caller = "grace"
	a.Get("/limited").Send().AssertStatus(http.StatusOK)
}

// The window must roll, or a limiter is a permanent ban.
func TestTheWindowRolls(t *testing.T) {
	a := boot(t, &Opts{Limit: 1, Window: 100 * time.Millisecond, Key: func(app.Context) string {
		return "rolling-" + t.Name()
	}})

	a.Get("/limited").Send().AssertStatus(http.StatusOK)
	a.Get("/limited").Send().AssertStatus(http.StatusTooManyRequests)

	// Two windows' worth, so the test does not race the boundary.
	time.Sleep(220 * time.Millisecond)
	a.Get("/limited").Send().AssertStatus(http.StatusOK)
}

// A limiter is a safety belt. A broken safety belt must not stop the car:
// refusing everything because the cache is unreachable turns a cache outage
// into a total outage.
//
// cache.Global is process-wide and every other test in this package installs
// one, so it has to be removed explicitly — a test cannot un-configure the
// cache simply by omitting the provider.
func TestACacheFailureAllowsTheRequest(t *testing.T) {
	previous := cache.Global()
	cache.SetGlobal(nil)
	t.Cleanup(func() { cache.SetGlobal(previous) })

	ctx := context.Background()

	// Well past any limit: with no cache there is no counter, so every call
	// must still be allowed.
	for i := 0; i < 5; i++ {
		allowed, _, err := Allow(ctx, "no-cache", 1, time.Minute)
		if !allowed {
			t.Fatalf("call %d was refused with no cache available", i+1)
		}
		if err == nil {
			t.Error("Allow reported no error despite having no cache; the caller cannot tell it is unprotected")
		}
	}
}

// A limit of zero refuses everything, which is a usable way to turn a feature
// off. A window of zero cannot be enforced and must not refuse.
func TestDegenerateLimits(t *testing.T) {
	ctx := context.Background()

	if allowed, _, _ := Allow(ctx, "zero-limit", 0, time.Minute); allowed {
		t.Error("a limit of 0 allowed a request")
	}
	if allowed, _, _ := Allow(ctx, "zero-window", 10, 0); !allowed {
		t.Error("a window of 0 refused a request")
	}
}

// X-Forwarded-For is client-supplied. Trusting it unconditionally lets anyone
// bypass an IP limit by sending a different value on every request, which is
// worse than having no limiter.
func TestForwardedHeadersAreIgnoredUnlessProxyIsTrusted(t *testing.T) {
	t.Cleanup(func() { TrustProxy(false) })

	a := apptest.New(t,
		apptest.WithConfig(cacheConfig()),
		apptest.WithProviders(cacheProvider()),
		apptest.WithRoutes(func(a app.App) {
			a.Router().Get("/ip", func(c app.Context) error {
				return c.JSON(app.M{"ip": ClientIP(c)})
			})
		}),
	)

	TrustProxy(false)
	untrusted := a.Get("/ip").Header("X-Forwarded-For", "1.2.3.4").Send().
		AssertStatus(http.StatusOK).JSONValue("ip")
	if untrusted == "1.2.3.4" {
		t.Error("X-Forwarded-For was believed without TrustProxy, so any IP limit is bypassable")
	}

	TrustProxy(true)
	trusted := a.Get("/ip").Header("X-Forwarded-For", "1.2.3.4, 10.0.0.1").Send().
		AssertStatus(http.StatusOK).JSONValue("ip")
	if trusted != "1.2.3.4" {
		t.Errorf("with TrustProxy the client IP = %v, want the left-most entry", trusted)
	}
}

func TestTrimAndIndexHelpers(t *testing.T) {
	if got := trimSpace("  1.2.3.4\t"); got != "1.2.3.4" {
		t.Errorf("trimSpace = %q", got)
	}
	if got := indexByte("a,b", ','); got != 1 {
		t.Errorf("indexByte = %d", got)
	}
	if got := indexByte("ab", ','); got != -1 {
		t.Errorf("indexByte with no match = %d", got)
	}
}

# lemmego/ratelimit

A fixed-window rate limiter for Lemmego applications, over the application's
cache.

```go
import "github.com/lemmego/ratelimit"

r.Post("/login", ratelimit.Middleware(ratelimit.PerIP(5, time.Minute)), handlers.Login)

if ok, retryAfter, err := ratelimit.Allow(ctx, "vote:"+userID, 30, time.Minute); !ok {
    …
}
```

## Install

```bash
go get github.com/lemmego/ratelimit
```

It needs `lemmego/cache` registered. Nothing else — there is no provider,
because there is no state to build: the counters live in the cache and the
middleware is a function.

## Read this before you deploy it

**It is a fixed-window counter over the cache, so it is shared across
processes only when the cache is.** With the memory store and three
instances behind a load balancer, a limit of 5 allows 15. With Redis it
allows 5.

That is not a bug to be fixed later; it is the honest consequence of where the
counters live, and it is the first thing to check when a limit "isn't
working".

| Cache store | Scope of a limit |
|---|---|
| memory | this process |
| file | this host |
| **redis** | **the cluster** |

A fixed window also means a caller can send `limit` requests at the end of one
window and `limit` again at the start of the next — up to twice the limit
across a window boundary. For login and signup that is irrelevant. For
anything where it matters, the honest answer is a sliding window, which this
package deliberately does not implement.

## The four key strategies

```go
ratelimit.PerIP(5, time.Minute)      // anonymous: login, register, password reset
ratelimit.PerUser(30, time.Minute)   // signed-in actions: vote, comment, post
ratelimit.PerRoute(10, time.Minute)  // route and caller together
&ratelimit.Opts{Limit: 10, Window: time.Minute, Key: myKey, Message: "…"}
```

**Prefer `PerUser` for anything only a signed-in person can do.** An IP is
shared by an office, a university and a mobile carrier's NAT; limiting those
as one caller punishes the wrong people. `PerUser` falls back to the IP for an
anonymous request, so it is safe on a mixed route.

**`PerRoute` keys on method, path and caller together**, so somebody near
their limit on one action is not refused a different one.

## `X-Forwarded-For` is ignored until you say otherwise

```go
ratelimit.TrustProxy(true)   // only when a proxy you control sets the header
```

`ClientIP` reads `X-Forwarded-For` **only** after this is enabled, because the
header is client-supplied. Trusting it unconditionally lets anyone bypass an
IP limit by sending a different value on every request — which is worse than
having no limiter, since it also looks like one is working.

Enable it only when something you control overwrites the header at the edge.

## A refusal says when to retry

```http
HTTP/1.1 429 Too Many Requests
Retry-After: 43
```

That is not a courtesy. Without it a client that retries immediately turns one
refused request into a tight loop, and the limiter becomes the load it was
meant to prevent.

`Allow` returns the same information for a non-HTTP caller:

```go
ok, retryAfter, err := ratelimit.Allow(ctx, "vote:"+userID, 30, time.Minute)
```

## A cache failure allows the request

If the cache is unreachable, `Allow` returns `true` and logs. The alternative
is a Redis outage turning into a total outage: every login, every signup,
every vote refused because a counter could not be read.

This is a decision, not an oversight, and it is the right one for the things a
limiter usually guards. If you are limiting something where failing open is
unacceptable, check the error — it is returned, not swallowed.

## Keys are namespaced

Every counter is stored under `ratelimit:` (`KeyPrefix`), so flushing the
limiter cannot flush anything else and a stray key cannot be mistaken for
application data.

## A note on the window

`cache.Increment` takes no TTL, so a window cannot be created by incrementing.
The window is seeded with `cache.Add(ctx, key, 0, window)` — which only writes
if the key is absent — and then incremented. That ordering is what makes the
window roll rather than extend: the TTL is set once, by whoever arrives first,
and every later hit in that window inherits it.

## Deliberate omissions

No sliding windows, no token buckets, no per-route configuration files. Each
would be a reasonable addition; none of them is needed to stop the abuse a
public form attracts on its first day, and the fixed-window counter is the
version whose behaviour you can predict without reading it.

## Testing

```bash
go test ./...
GOWORK=off go test ./...   # as a published module
```

The suite asserts the limit holds across requests, that a refusal says when to
retry, that callers are limited separately, that the window actually rolls,
that a cache failure allows the request, and that forwarded headers are
ignored until `TrustProxy`.

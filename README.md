# lemmego/ratelimit

A fixed-window rate limiter for Lemmego applications, over the application's
cache. Reach for it on any route a stranger can reach — sign-in, registration,
password reset — and on any signed-in action somebody could script.

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

You need `lemmego/cache` registered. Nothing else — there is no provider to
add, because there is no state to build: the counters live in your cache and
the middleware is a function.

## Scope and limits

A limit is shared across your processes only when the cache is. With the
memory store and three instances behind a load balancer, a limit of 5 allows
15; with Redis it allows 5. If a limit seems not to be working, check this
first.

| Cache store | Scope of a limit |
|---|---|
| memory | this process |
| file | this host |
| **redis** | **the cluster** |

A fixed window also lets somebody send `limit` requests at the end of one
window and `limit` again at the start of the next — up to twice the limit
across a window boundary. For sign-in and registration that does not matter.
If you need a strict rate, put the limiter behind something that enforces one.

## Choosing a key strategy

```go
ratelimit.PerIP(5, time.Minute)      // anonymous: login, register, password reset
ratelimit.PerUser(30, time.Minute)   // signed-in actions: vote, comment, post
&ratelimit.Opts{Limit: 10, Window: time.Minute, Key: myKey, Message: "…"}
```

**Prefer `PerUser` for anything only a signed-in person can do.** An IP is
shared by an office, a university and a mobile carrier's NAT, so limiting
those as one caller punishes the wrong people. `PerUser` falls back to the IP
for an anonymous request, so you can put it on a route that serves both.

`PerRoute` is an alias for `PerUser`, kept because the name reads well at a
call site where the limit is about the route rather than the person.

## How keys are built

Both strategies key on **the caller and the route together**, so each limit
counts its own action and nothing else.

The route is the **matched pattern**, not the request path, which matters for
any route carrying a parameter. Keying on the path would give
`/launch/a/upvote` and `/launch/b/upvote` separate counters, turning "20
upvotes an hour" into "20 an hour per launch" — a limit somebody steps around
by choosing a different target.

Having the route in the key is also what keeps your limits independent. The
window slot is part of the stored key, so without it every limit sharing a
*window length* would share one counter while each route compared that counter
against its own threshold. Nine hourly limits would become one hourly budget,
and the first route to exhaust it would lock a person out of the other eight
— while the refusal quoted a limit the caller had not reached.

If you do want one budget spanning several routes, set `Opts.Key` to something
that does not vary by route, and leave a comment saying why.

## `X-Forwarded-For` is ignored until you say otherwise

```go
ratelimit.TrustProxy(true)   // only when a proxy you control sets the header
```

`ClientIP` reads `X-Forwarded-For` **only** after you enable this, because the
header is client-supplied. Trusting it unconditionally lets anyone bypass an
IP limit by sending a different value on every request — which is worse than
having no limiter, because it also looks like one is working.

Enable it only when something you control overwrites the header at the edge.

## What a refused request looks like

```http
HTTP/1.1 429 Too Many Requests
Retry-After: 43
```

`Retry-After` is there so a client that retries does not retry immediately.
Without it one refused request becomes a tight loop, and the limiter turns
into the load it was meant to prevent.

`Allow` returns the same information for a caller that is not serving HTTP:

```go
ok, retryAfter, err := ratelimit.Allow(ctx, "vote:"+userID, 30, time.Minute)
```

## Behaviour when the cache is down

If the cache is unreachable, `Allow` returns `true` and logs. The alternative
is a Redis outage becoming a total outage: every sign-in, every registration,
every vote refused because a counter could not be read.

That is the right trade for the things a limiter usually guards. If you are
limiting something where allowing the request is unacceptable, check the error
— it is returned, not swallowed.

## Keys are namespaced

Every counter is stored under `ratelimit:` (`KeyPrefix`), so flushing the
limiter cannot flush anything else of yours, and a stray key cannot be
mistaken for application data.

## How the window rolls

`cache.Increment` takes no TTL, so a window cannot be created by incrementing.
The window is seeded with `cache.Add(ctx, key, 0, window)` — which writes only
if the key is absent — and then incremented. That ordering is what makes the
window roll rather than extend: the TTL is set once, by whoever arrives first,
and every later hit in that window inherits it.

## Limitations

No sliding windows, no token buckets, no per-route configuration files. Each
would be a reasonable addition; none is needed to stop the abuse a public form
attracts on its first day, and a fixed-window counter is the one whose
behaviour you can predict without reading its source.

## Testing

```bash
go test ./...
GOWORK=off go test ./...   # as a published module
```

The suite asserts the limit holds across requests, that a refusal says when to
retry, that callers are limited separately, that the window rolls, that a
cache failure allows the request, and that forwarded headers are ignored until
`TrustProxy`.

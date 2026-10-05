# Contributing

Thanks for your interest! Bug reports, fixes and ideas are all welcome.

## Getting started

You need Go (see `go.mod` for the version). Nothing else: the frontend has no build step.

```sh
git clone https://github.com/sainad2222/hopclip && cd hopclip
make run     # http://localhost:8080, user admin / devpassword
make test
make lint
```

Browsers accept `Secure` cookies on `localhost`, so the default config works for local development. To test from a phone on your LAN over plain HTTP, set `COOKIE_SECURE=false` and `LISTEN_ADDR=:8080`.

## Layout

```
cmd/hopclip/        entrypoint: serve, user CLI, healthcheck, version
internal/config/      environment variables → Config
internal/auth/        argon2id password hashing
internal/store/       SQLite schema, migrations and queries; blob paths
internal/server/      HTTP routes, middleware, handlers, SSE hub, rate limiter
web/static/           index.html, app.js, style.css (embedded into the binary)
deploy/               Caddyfile
docs/                 deployment and API docs, README images
```

## Guidelines

- **Keep it small.** The goal is one binary, one volume, no external services. New dependencies need a good reason.
- **Tests.** Server behaviour is tested end to end in `internal/server/server_test.go` against a real TLS test server and SQLite. Add a test with every fix or feature.
- **Frontend.** Plain JS modules, no frameworks. The page runs under a strict CSP: no inline `<script>`, no `style=""` attributes, no `innerHTML` with data. Render user content with `textContent`. Check changes on a phone-sized screen and in dark mode.
- **Schema changes.** Append a new entry to `migrations` in `internal/store/store.go`; never edit an existing one.
- **Comments** explain *why*, not *what*.
- **Commits.** A short imperative subject line, e.g. `Add clip pinning`. One logical change per commit.

## Pull requests

1. Open an issue first for anything bigger than a small fix, so we can agree on the approach.
2. Make sure `make lint test` passes.
3. Describe what changed and how you tested it.

## Code of conduct

Be kind and assume good intent. This project follows the [Contributor Covenant](https://www.contributor-covenant.org/version/2/1/code_of_conduct/).

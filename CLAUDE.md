# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Telegram alerting bot in Go. Services register their users through a signed `/start` link and send
them messages by user or group id or alias over a REST API. Data lives in PostgreSQL. [README.md](README.md) covers
the registration flow, the API and the queue; [deploy/DEPLOY.md](deploy/DEPLOY.md) covers production.

Code comments, docs, the OpenAPI spec and bot texts are in Russian. Log messages and API error
strings are in English.

## Commands

```bash
go test ./...                                           # all tests
go test ./internal/api -run TestSpecCoversEveryRoute -v # one test
go vet ./...
gofmt -l cmd internal pkg
go run ./cmd/blind-postman [-config path]               # needs config.ini, PostgreSQL and Telegram
go run ./cmd/genlink -bot my_alert_bot -mode token -ttl 1h
go run ./cmd/blind-postman migrate status               # also: create NAME, version, up, down, down-to N
./3_build-app.sh                                        # blindpostman-bot, static Linux x86-64
```

- Storage tests need PostgreSQL and are skipped without it:
  `TEST_DATABASE_URL="postgres://postman:postman@localhost:5432/postman?sslmode=disable" go test ./internal/storage -v`.
  So the real queue SQL is not exercised by a plain `go test ./...`.
- `go test -race` does not work on the Windows dev machine: it has no C compiler.
- With `core.autocrlf=true`, `gofmt -l` also lists files git checked out with CRLF. Check one with
  `tr -d '\r' < file.go | gofmt -l`.
- There is no linter config and no Makefile.

## Architecture

`cmd/blind-postman` runs three things in one process, and PostgreSQL is what connects them:

- `internal/api` — REST API. It only writes to the queue and never calls Telegram.
- `internal/delivery` — dispatcher that drains the queue and sends through `internal/tgbot`.
- `internal/tgbot` — long polling for `/start`, `/stop`, `/status`, `/myid`, `/help`, plus `Send`.

`api`, `delivery` and `tgbot` depend on small interfaces (`Store`, `Bot`, `Sender`), so their tests
use fakes.

### Message path

`POST /api/v1/messages` inserts into `messages` and answers `202`. The recipient is exactly one of
`user_id`, `user_alias`, `group_id` or `group_alias`. For a group it inserts one row per active member
in a single statement (`enqueueGroup`), so a retry cannot double part of the group. A database trigger
sends `NOTIFY`,
the dispatcher wakes up and claims rows; polling (`queue_poll_interval`) picks up retries. Things to
keep true when changing `internal/storage/queue.go` or `internal/delivery`:

- Only the head of each subscriber's queue can be claimed, which keeps per-chat order.
- Claims use `FOR UPDATE SKIP LOCKED` with a lease (`locked_until`). The lease must exceed
  `LimiterWait + SendTimeout`.
- Delivery is at-least-once: a crash between sending and recording the result resends after the lease.
- A 429 pauses sending for `retry_after` and returns the message without spending an attempt.
  The global rate limit and this pause live in process memory.
- The claim loop runs up to tens of times per second, so anything in it must not read the whole queue.
  Periodic work belongs in `Dispatcher.housekeep`.

Run one instance per `bot_token`: the queue allows replicas, but Telegram allows one poller per token
(`409 Conflict`).

### Registration

`pkg/startlink` builds and verifies the `/start` parameter: 30 bytes (version, mode, expiry, nonce,
truncated HMAC-SHA256 of `link_shared_secret`), 40 characters of base64url. It is public API for Go
services, so changing the format breaks links they generate themselves. One-time use is enforced by
`registration_nonces`. Callback codes are stored only as SHA-256 hashes.

`recipients` is the only list of subscribed users. The link is just one way in, for partner systems
that know the shared secret; a row can also be inserted by hand, so never make the link a requirement.
The Go type `Subscriber`, the `subscriber_id` columns and the `/api/v1/subscribers` paths all refer to
this table.

In `recipients`, `id` is the UUID token (`Subscriber.Token`; `user_id` in `POST /api/v1/messages`, `token`
in the other API methods), and `internal_id` is the bigint primary key that the `subscriber_id` columns
reference (`Subscriber.ID`). Groups reference `id`.

`[telegram] developer_id` names one user the bot makes exceptions for. For now that means a plain
`/start` adds them to `recipients` without a link; new exceptions should check `Service.developerID`.

### Configuration

`internal/config` reads `config.ini` with its own INI parser: comments only at line start, `;` and `#`
inside values are literal, outer double quotes are stripped, errors report only the line number so
secrets stay out of logs. New options go into `config.example.ini` too (`TestExampleIsValid`).

`pkg/winnow` is a port of FreePascal `D:\4Delphi\AniCommon\awinnow.pas` and `TangleText` from
`Autils.pas`, used for `{HEX}` database passwords; a value without braces is plain text. It must stay
byte-compatible with the Pascal code, including FPC quirks. The expected values in its tests come from
compiled FPC.

## Keeping things in sync

- **API changes:** routes live in the `routes()` table in `internal/api/api.go`, and
  `internal/docs/openapi.yaml` is written by hand. Tests fail if a route and the spec disagree, or if
  the fields of the types listed in `TestSpecSchemasMatchTypes` differ from their schemas. Add new
  request and response types to that list. The anonymous request structs in `redeem` and
  `resolveToken` are not covered.
- **Nginx:** only `/api/` and `/doc/` are proxied, so a new public path needs a `location` in
  `deploy/blindpostman-bot.nginx.conf`. `client_max_body_size` mirrors `maxBodySize`, and
  `proxy_read_timeout` must stay just above `requestTimeout`.
- **`/doc`:** served by `docs.Wrap` in front of `http.TimeoutHandler`, without a token. Swagger UI is
  embedded in the binary and must not load anything from external hosts.
- **Migrations:** goose SQL files in `internal/storage/migrations` are embedded and applied at startup.
  Never edit an applied file; add a new migration.
- **systemd unit:** `deploy/blindpostman-bot.service` runs in unprivileged LXC, so it must not use
  options that need namespaces (`status=226/NAMESPACE`). Those belong in the optional section of
  DEPLOY.md.

## Repository

- Commit to the current branch, usually `main`. Create a branch only when asked.
- `config.ini`, `.mcp.json` and `gittoken` hold real secrets and are gitignored. The `postman-prod` MCP
  server in `.mcp.json` points at the production database.
- `1_git-pull.sh` and `2_git-push.sh` authenticate with `gittoken`.
- Build scripts exist as `.sh` and `.cmd` pairs; change both. `core.autocrlf=true` is set.

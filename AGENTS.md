# Working rules for this repository

Fishplate keeps one canonical bug record across Asana, GitHub and Slack. This file is for the
implementing agent. The design is `docs/DESIGN.md`; where this file and the design disagree, the
design wins and this file should be corrected.

## Language and dependencies

- Go 1.27, module `github.com/StevenGann/fishplate`.
- Standard library first. A dependency needs a reason in the commit message.
- SQLite driver: `modernc.org/sqlite` (pure Go, no cgo) so the binary stays static and the image
  stays distroless. Do not add `mattn/go-sqlite3`.
- No provider SDKs in v0.1. The GitHub, Asana and Slack clients are plain `net/http` against the
  documented endpoints.

## Commands

```
make build     # go build ./cmd/fishplate
make test      # go test ./...
make vet       # go vet ./...
make run       # go run ./cmd/fishplate serve
make check     # vet + test + build, the gate before any commit
```

A change is not done until `make check` passes on this machine.

## Layout

```
cmd/fishplate/        main, subcommand wiring
internal/config/      precedence, parsing, redaction, effective-config printing
internal/store/       sqlite, migrations, queries
internal/web/         HTTP handlers, health, metrics
internal/engine/      change application, ownership, state machine
internal/connectors/  github/ asana/ slack/
internal/mcp/         tools and transports
internal/rules/       schema, validation, templates
api/                  OpenAPI for /api/v1
deploy/               Helm chart, compose demo
docs/                 design and phase notes
```

## Rules that are not negotiable

- **No secrets in the repository.** No API keys, tokens, real domains, real hostnames, internal IP
  addresses or customer names. Configuration examples use placeholders.
- **Forward-only migrations**, numbered, applied at startup under a lock. Never edit an applied
  migration.
- **Configuration precedence:** command-line flags, then `FISHPLATE_*` environment variables, then
  the config file, then defaults. `fishplate config show --effective` prints the resolved values
  with secrets redacted.
- **Errors over HTTP** are RFC 9457 `application/problem+json`.
- **No silent fallbacks.** If a required setting is missing or a connector cannot initialise, refuse
  to start or fail the request with a reason. Never degrade to a permissive default.
- **Logs are JSON** on stderr, with `request_id` where one exists.
- **Tests are part of the change.** New behaviour gets a test that fails without it.

## Scope discipline

v0.1 is: GitHub read and write, Asana events read and write, Slack notifications, the dashboard,
and a read-only MCP server. Anything outside `docs/DESIGN.md` section 3 is not in scope, however
small the request looks.

## Commits

The agent does not commit. Leave the worktree dirty, report what changed and what was verified, and
let the operator review the diff.

## Environment

Work only inside the repository. Scratch files go under `.scratch/` in the repo, which is
gitignored, not `/tmp`: the sandbox refuses writes outside the project directory and the run stalls
when a command tries.

The Docker daemon is not reachable from the development container. Write the `Dockerfile` and the
compose file, but do not expect `docker build` to run here; CI is where the image is proven. Say so
in the report rather than leaving it untested and unmentioned.

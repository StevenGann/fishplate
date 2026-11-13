# Phase 0 — skeleton

From `docs/DESIGN.md` section 22. Two handoffs, each ending in a state the operator can run.

## 0a — application shell

Goal: a binary that starts, reports its version, serves health endpoints and prints its effective
configuration. No database, no connectors.

Deliverables:

1. `go.mod` with module `github.com/StevenGann/fishplate`, Go 1.27. Allowed dependency:
   `gopkg.in/yaml.v3` for the config file. Nothing else.
2. `cmd/fishplate/main.go` wiring three subcommands:
   - `version` prints `fishplate <version> (<commit>, <date>, <go version>)` and exits 0.
   - `serve` starts the HTTP server.
   - `config show --effective` prints resolved configuration as YAML with secrets replaced by
     `<set>` or `<unset>`, never the value.
3. `internal/config`:
   - Precedence: flags, then `FISHPLATE_*` environment variables, then the config file
     (`/etc/fishplate/config.yaml` by default, overridable with `--config`), then defaults.
   - Fields for 0a: `http.addr` (default `:8080`), `log.level` (default `info`), `log.format`
     (`json`), `db.path` (default `/data/fishplate.db`), and a `secrets` section whose values are
     always redacted on output.
   - Unknown keys in the config file are an error, not a warning.
4. `internal/web`:
   - `GET /healthz` returns 200 `{"status":"ok"}`.
   - `GET /readyz` returns 200 when the process is serving; in 0a it does not check the database.
   - `GET /metrics` returns Prometheus text with `fishplate_build_info{version,commit}` and a
     request counter.
   - Middleware assigns a request id, logs one JSON line per request, and recovers panics into a
     500 problem+json response.
5. `internal/logging` or equivalent: JSON handler on stderr via `log/slog`, level from config.
6. `Makefile` with `build`, `test`, `vet`, `run`, `check` targets. `build` writes `./bin/fishplate`
   and injects version, commit and date with `-ldflags`.
7. Tests: table-driven tests for config precedence, unknown-key rejection, secret redaction, and
   handler tests for the three endpoints including the panic path.

Acceptance, all run by the operator:

```
make check
./bin/fishplate version
./bin/fishplate serve &            # then:
curl -s -o /dev/null -w '%{http_code}' localhost:8080/healthz   # 200
curl -s localhost:8080/healthz                                   # {"status":"ok"}
curl -s localhost:8080/readyz                                    # 200
curl -s localhost:8080/metrics | grep fishplate_build_info
FISHPLATE_HTTP_ADDR=:9099 ./bin/fishplate config show --effective | grep 9099
```

## 0b — store, migrations, container

Goal: the same binary with a writable SQLite database, a migration runner, a distroless image and
CI.

Deliverables:

1. `internal/store`: `modernc.org/sqlite`, WAL mode, `busy_timeout`, one writer, embedded
   forward-only migrations (`0001_init.sql` creating a `schema_migrations` table and nothing else).
2. `fishplate migrate` and `fishplate migrate --dry-run`; `serve` applies pending migrations at
   startup under an advisory lock and refuses to start if the schema version is newer than the
   binary.
3. `/readyz` returns 503 until the database is writable, and checks with a real write.
4. Multi-stage `Dockerfile`: `golang:1.27` build stage, `gcr.io/distroless/static-debian12:nonroot`
   runtime, no shell, `/data` owned by the runtime user.
5. `.dockerignore`, `deploy/compose/docker-compose.yml` with a named volume.
6. CI at `.github/workflows/ci.yml`: `go vet`, `go test -race ./...`, `go build`, and a Docker
   build on pull requests and main.
7. Tests: migration applies once and is idempotent; a newer-schema database is refused; readiness
   fails when the path is unwritable.

Acceptance:

```
make check
docker build -t fishplate:dev .
docker run --rm -v fishplate-data:/data -p 8080:8080 fishplate:dev serve &
curl -s -o /dev/null -w '%{http_code}' localhost:8080/readyz    # 200 after migration
docker run --rm fishplate:dev version                            # prints, exits 0
```

## Out of scope for Phase 0

Connectors, rules, the engine, the dashboard and MCP. Phase 1 starts with GitHub read-only polling.

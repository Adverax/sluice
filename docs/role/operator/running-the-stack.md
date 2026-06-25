# Running the Stack

## Full demo stack (recommended for first look)

Run the entire stack — gateway, Postgres, Redis, Prometheus, and Grafana — with one
command:

```sh
make up
```

This builds the gateway image (multi-stage Dockerfile, distroless final image), applies
database migrations, and wires Prometheus scraping and Grafana dashboard provisioning
automatically.

Service endpoints after `make up`:

| Service | URL |
|---------|-----|
| Gateway API | http://localhost:8080 |
| Prometheus metrics | http://localhost:8080/metrics |
| Prometheus UI | http://localhost:9090 |
| Grafana | http://localhost:3000 (anonymous admin) |

Tear the stack down and remove all volumes:

```sh
make down
```

## Dev loop (host-run gateway)

For iterative development, start only the backing services and run the gateway
as a host process:

```sh
make run
```

This starts Postgres and Redis in Docker (no port conflict), then runs
`go run ./cmd/gateway` on your host. Use this when you are changing code and want
a fast edit-run cycle.

Stop the backing services when you are done:

```sh
make infra-down
```

## Infrastructure only

If you want the backing services running but do not want to start the gateway
(for example, to run integration tests separately):

```sh
make infra        # start postgres + redis
make infra-down   # stop them
```

## Container image

The gateway image is built from the repo-root `Dockerfile`. It uses a two-stage
build:

1. A `golang:1.25-alpine` build stage compiles a static binary with CGO disabled.
2. A `gcr.io/distroless/static-debian12:nonroot` runtime stage carries only the
   binary and the SQL migrations. There is no shell or package manager in the final
   image.

The container exposes port 8080 and runs as a non-root user. There is no
healthcheck baked into the compose definition for the gateway container — the
distroless image has no shell or `curl`. Use the `/healthz` and `/readyz` HTTP
endpoints instead (see [health-and-readiness.md](health-and-readiness.md)).

## Log output

Logs are structured JSON by default. To switch to human-readable text during
development, set `GATEWAY_LOG_FORMAT=text`. See
[configuration-reference.md](configuration-reference.md) for all log-related
variables.

## Tail logs

```sh
make stack-logs    # full stack (gateway + infra)
make logs          # infra only (postgres + redis)
```

# Health and Readiness Probes

The gateway exposes two HTTP probe endpoints. Wire them separately — they serve
different purposes.

## Liveness: GET /healthz

Returns `200 OK` with `{"status":"ok"}` as long as the process is running.

```sh
curl http://localhost:8080/healthz
# {"status":"ok"}
```

Use this as the **liveness probe** in Kubernetes or any load balancer. A non-200
response here means the process has crashed and should be restarted.

This endpoint never contacts Redis or Postgres. It always answers 200 while the
process is alive.

## Readiness: GET /readyz

Returns `200 OK` when both Redis and Postgres are reachable. Returns `503 Service
Unavailable` when either dependency is down, with a per-dependency status map in
the body.

```sh
# All dependencies healthy
curl http://localhost:8080/readyz
# {"status":"ok","dependencies":{"postgres":"ok","redis":"ok"}}

# Redis down
curl http://localhost:8080/readyz
# {"status":"unavailable","dependencies":{"postgres":"ok","redis":"dial tcp: ..."}}
```

Use this as the **readiness probe**. A 503 response means the instance should
stop receiving traffic until its dependencies recover. Do not restart on a 503 —
the process is alive; its dependencies are not.

Each dependency check runs concurrently under a deadline controlled by
`GATEWAY_HEALTH_CHECK_TIMEOUT` (default 3s). A slow dependency cannot starve the
others. See [configuration-reference.md](configuration-reference.md).

## Kubernetes probe configuration (example)

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: 8080
  initialDelaySeconds: 5
  periodSeconds: 10

readinessProbe:
  httpGet:
    path: /readyz
    port: 8080
  initialDelaySeconds: 5
  periodSeconds: 10
  failureThreshold: 3
```

## Tuning the probe timeout

The `GATEWAY_HEALTH_CHECK_TIMEOUT` variable controls how long each individual
dependency check (Redis ping, Postgres pool acquire) may take before it is
considered failed. The default is `3s`. Lower it if your load balancer expects
faster probe responses; raise it if your dependency connections are slow to
establish on startup.

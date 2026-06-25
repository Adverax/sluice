# Graceful Shutdown

## How shutdown works

Send `SIGINT` or `SIGTERM` to the gateway process. The shutdown sequence is:

1. **Stop accepting new connections.** The HTTP server stops listening immediately.
2. **Drain in-flight requests.** The gateway waits for all active requests to complete,
   up to `GATEWAY_SHUTDOWN_TIMEOUT` (default 30s).
3. **Run post-drain hooks.** After the HTTP drain (or after the drain deadline elapses),
   each registered shutdown hook runs with its own independent deadline of
   `GATEWAY_SHUTDOWN_HOOK_TIMEOUT` (default 5s). The metering flush is the main hook.
4. **Log the summary and exit.**

## Forced shutdown

If in-flight requests do not all complete within `GATEWAY_SHUTDOWN_TIMEOUT`, the
gateway does not hang. It logs the number of unfinished requests:

```
forced shutdown: 3 requests unfinished
```

and then proceeds to the post-drain hooks. The process exits after the hooks complete.
Forced shutdown is not an error — it is the correct behaviour when a deadline elapses.

## Metering flush on shutdown

Usage events that are still buffered in memory when shutdown begins are flushed to
Postgres during the post-drain hook phase. The metering worker receives a dedicated
budget of `GATEWAY_SHUTDOWN_HOOK_TIMEOUT` (default 5s) for this flush, independent
of whether the HTTP drain timed out.

After the hooks complete, the gateway logs a single summary line:

```
shutdown complete: drained N requests, flushed M usage events
```

This tells you exactly how many in-flight HTTP requests were drained and how many
buffered usage events were written to Postgres before exit.

## Tuning shutdown timing

| Situation | Adjustment |
|-----------|-----------|
| Requests take longer than 30s and you see forced-shutdown warnings | Increase `GATEWAY_SHUTDOWN_TIMEOUT` |
| Metering flush times out during shutdown | Increase `GATEWAY_SHUTDOWN_HOOK_TIMEOUT` |
| Shutdown takes too long for your deployment pipeline | Decrease `GATEWAY_SHUTDOWN_TIMEOUT`; accept more forced shutdowns |

## Container stop

When a container orchestrator (e.g. Kubernetes) stops a pod, it sends SIGTERM. The
gateway responds as described above. Set the pod's `terminationGracePeriodSeconds`
to at least `GATEWAY_SHUTDOWN_TIMEOUT + GATEWAY_SHUTDOWN_HOOK_TIMEOUT` (default
30s + 5s = 35s) to give the gateway its full budget before the orchestrator sends
SIGKILL.

```yaml
# Example Kubernetes deployment snippet
spec:
  terminationGracePeriodSeconds: 40   # >= GATEWAY_SHUTDOWN_TIMEOUT + GATEWAY_SHUTDOWN_HOOK_TIMEOUT
  containers:
    - name: gateway
      env:
        - name: GATEWAY_SHUTDOWN_TIMEOUT
          value: "30s"
        - name: GATEWAY_SHUTDOWN_HOOK_TIMEOUT
          value: "5s"
```

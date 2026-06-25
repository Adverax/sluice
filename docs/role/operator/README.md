# Operator Guide

This documentation is for the **Operator** role (ACT-002): an SRE or DevOps engineer
who runs, monitors, tunes, and scales the sluice gateway.

As an operator you own the process lifecycle (start, stop, scale), watch the service
stay healthy under load, and tune environment variables when traffic patterns change.
You do not write application code; you work with the running system.

## What you can do

| Task | Guide |
|------|-------|
| Start, stop, and run the demo stack | [running-the-stack.md](running-the-stack.md) |
| Wire liveness and readiness probes | [health-and-readiness.md](health-and-readiness.md) |
| Read metrics and use the Grafana dashboard | [monitoring-and-metrics.md](monitoring-and-metrics.md) |
| Tune every `GATEWAY_*` environment variable | [configuration-reference.md](configuration-reference.md) |
| Understand overload behaviour, circuit breaker, and scaling | [operating-under-load.md](operating-under-load.md) |
| Understand graceful shutdown and what gets flushed | [graceful-shutdown.md](graceful-shutdown.md) |

## Quick orientation

The gateway is a **stateless HTTP process**. All shared state lives in Redis (rate
limiting, response cache) and Postgres (usage events). This means you can run
multiple gateway instances behind a load balancer without any instance coordination;
they share rate-limit counters and cache entries automatically.

The binary listens on `:8080` by default. Every behaviour listed in the guides
above is controlled by `GATEWAY_*` environment variables with safe defaults — you
can boot the process with no environment configuration and it will work.

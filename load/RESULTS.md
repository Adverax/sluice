# Load test results

This file records two complementary measurements of gateway overhead (NFR-001:
p95 overhead ≤ 20 ms):

1. **In-process overhead bench** — a real, runnable Go measurement of the
   composed handler chain against a 0-latency mock provider. Because the mock
   adds no latency, the measured per-request wall-clock time **is** the gateway's
   own overhead. This requires no deployed stack and is run on every commit.
2. **Full-stack k6 load run** — the `load/scenario.js` k6 scenario driven over
   the network against the `make up` stack (gateway + postgres + redis). This is
   the end-to-end NFR-001/NFR-002 figure.

> **Honesty note:** numbers below are only filled in when they were actually
> measured, and are labelled with the exact environment. The k6 full-stack table
> is left as an explicit `TODO` because k6 was not installed in the authoring
> environment — it must be measured on the declared hardware via `make load`.
> No load figures are fabricated.

---

## 1. In-process overhead bench (REAL — measured)

Test: `TestBenchGateway_p95OverheadUnder20ms` in `load/bench_test.go`
(`go test -run TestBenchGateway_p95OverheadUnder20ms ./load/`), 5,000 requests
through the full server handler chain (router → OpenAPI validation → strict
handler → worker pool → 0-latency mock), measuring per-request overhead.

### Environment

| Field        | Value                                   |
|--------------|-----------------------------------------|
| CPU          | Apple M5 Pro, 18 cores                   |
| RAM          | 48 GB                                    |
| OS           | macOS 26.5.1 (arm64)                     |
| Go version   | go1.26.1                                 |
| Mock latency | 0 ms (overhead-only)                     |
| Worker pool  | 100                                      |

### Results

| Mode                         | p50      | p95       | p99       | Target (p95) | Verdict |
|------------------------------|----------|-----------|-----------|--------------|---------|
| normal `go test`             | 5.96 µs  | 10.96 µs  | 54.67 µs  | ≤ 20 ms      | PASS    |
| `-race` (detector overhead)  | 54.83 µs | 66.54 µs  | 98.33 µs  | ≤ 20 ms      | PASS    |

The p95 gateway overhead is ~11 µs (normal) / ~67 µs (under the race detector) —
roughly **three orders of magnitude under the 20 ms NFR-001 budget** on this
machine. The strict 20 ms assertion lives in the test; under `-short` it relaxes
to a lenient ceiling so shared CI runners never flake (the real bound runs in a
normal `go test`).

---

## 2. Full-stack k6 load run (TODO — measure via `make load`)

> **TODO: measure on declared hardware via `make load`.** k6 was not installed
> in the authoring environment, so these rows are placeholders, NOT fabricated
> measurements. To populate:
>
> ```sh
> make up            # full demo stack: gateway + postgres + redis + prometheus + grafana
> # wait for the gateway to be ready:
> curl -fsS http://localhost:8080/readyz
> make load          # runs load/scenario.js via k6 against http://localhost:8080
> # or: k6 run -e BASE_URL=http://localhost:8080 load/scenario.js
> ```
>
> Note: use `make up` (full stack with the dockerised gateway on :8080), NOT
> `make run` (host dev loop — no load test should target a dev process).
>
> Then copy k6's end-of-run summary (`http_req_duration` percentiles + the
> per-status counts) into the table below and record the environment.

### Environment (to fill in)

| Field        | Value         |
|--------------|---------------|
| CPU          | TODO          |
| RAM          | TODO          |
| OS           | TODO          |
| Go version   | TODO          |
| Mock latency | 0 ms          |
| k6 version   | TODO          |

### Results (to fill in)

| Phase            | RPS   | p50    | p95    | p99    | error-rate | Notes                       |
|------------------|-------|--------|--------|--------|------------|-----------------------------|
| plateau          | ~3000 | TODO   | TODO   | TODO   | TODO       | p95 must be ≤ 20 ms (NFR-001)|
| 3× overload spike| ~9000 | TODO   | TODO   | TODO   | TODO       | only 200/429/503 (NFR-002)  |
| recovery         | ~3000 | TODO   | TODO   | TODO   | TODO       | accepts again post-load     |

`load/scenario.js` already encodes the NFR thresholds, so a `make load` run that
exits 0 has met them:

- `http_req_duration p(95) <= 20` (NFR-001 gateway overhead), and
- `bad_status count == 0` — every response was 200/429/503, no crash (NFR-002).

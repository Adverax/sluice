// k6 load scenario for the sluice gateway (NFR-001/NFR-002, AC-042/AC-043).
//
// Shape: ramp -> plateau (several-thousand RPS) -> 2-3x overload spike ->
// recovery, all against POST /v1/chat/completions backed by the mock provider
// (0ms upstream latency, so measured latency is gateway OVERHEAD).
//
// Run against the `make up` stack:
//   make up                       # gateway + postgres + redis + prometheus + grafana
//   k6 run -e BASE_URL=http://localhost:8080 load/scenario.js
//   # or: make load
//
// Thresholds (the run FAILS if violated):
//   - http_req_duration p95 <= 20ms over the whole test (NFR-001 gateway overhead).
//   - bad_status (anything other than 200/429/503) == 0 (NFR-002 graceful degradation).
//   - 0 crashes: the gateway must answer every request with a known status.
//
// The mock provider returns instantly, so http_req_duration is dominated by the
// gateway's own work (routing, validation, middleware chain) — i.e. the overhead.

import http from "k6/http";
import { check } from "k6";
import { Counter } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";

// bad_status counts any response outside the allowed {200, 429, 503} set, so a
// non-zero value means the gateway misbehaved under load (NFR-002).
const badStatus = new Counter("bad_status");

export const options = {
  // Use arrival-rate executors so we drive a TARGET RPS rather than a fixed VU
  // count — this is what makes "several-thousand RPS plateau + overload spike"
  // meaningful regardless of per-request latency.
  scenarios: {
    main: {
      executor: "ramping-arrival-rate",
      startRate: 100, // requests/sec at t0
      timeUnit: "1s",
      preAllocatedVUs: 200,
      maxVUs: 2000,
      stages: [
        { target: 1000, duration: "30s" }, // ramp up
        { target: 3000, duration: "30s" }, // ramp to plateau
        { target: 3000, duration: "2m" }, //  plateau (several-thousand RPS)
        { target: 9000, duration: "30s" }, // 3x overload spike
        { target: 9000, duration: "1m" }, //  sustained overload
        { target: 3000, duration: "30s" }, // back to plateau (recovery)
        { target: 0, duration: "30s" }, //    ramp down
      ],
    },
  },
  thresholds: {
    // NFR-001: gateway p95 overhead <= 20ms. Evaluated over the whole run; the
    // overload window naturally pushes the tail, which is the point of the test.
    "http_req_duration": ["p(95)<=20"],
    // NFR-002: every response must be a known status. Any other status is a bug.
    "bad_status": ["count==0"],
  },
};

const payload = JSON.stringify({
  model: "mock",
  messages: [{ role: "user", content: "ping" }],
});
const params = { headers: { "Content-Type": "application/json" } };

export default function () {
  const res = http.post(`${BASE_URL}/v1/chat/completions`, payload, params);

  const allowed = res.status === 200 || res.status === 429 || res.status === 503;
  if (!allowed) {
    badStatus.add(1);
  }

  check(res, {
    "status is 200/429/503 (no crash)": () => allowed,
  });
}

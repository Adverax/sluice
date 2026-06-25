// Steady-state latency probe for the sluice gateway (NFR-001 overhead).
//
// Unlike scenario.js (which drives an aggressive ramp→spike to test graceful
// degradation), this holds a CONSTANT arrival rate below the saturation knee so
// that http_req_duration reflects real end-to-end service time, not load-
// generator queueing. Sweep RATE to find the knee for your hardware.
//
//   k6 run -e BASE_URL=http://localhost:8080 -e RATE=500 -e DUR=30s \
//     --summary-trend-stats="avg,min,med,p(90),p(95),p(99),max" load/scenario-steady.js
//
// Keep dropped_iterations at 0 and vus below maxVUs — otherwise you are above
// the rig's ceiling and the latency is queueing, not gateway overhead. For a
// representative number, run k6 and the upstream on separate hosts from the
// gateway so the load generator does not compete with it for CPU.

import http from "k6/http";
import { check } from "k6";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const RATE = parseInt(__ENV.RATE || "500", 10);

export const options = {
  scenarios: {
    steady: {
      executor: "constant-arrival-rate",
      rate: RATE,
      timeUnit: "1s",
      duration: __ENV.DUR || "30s",
      preAllocatedVUs: 300,
      maxVUs: 1500,
    },
  },
};

const payload = JSON.stringify({
  model: "mock",
  messages: [{ role: "user", content: "ping" }],
});
const params = { headers: { "Content-Type": "application/json" } };

export default function () {
  const res = http.post(`${BASE_URL}/v1/chat/completions`, payload, params);
  check(res, { "status is 200": (r) => r.status === 200 });
}

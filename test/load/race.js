// k6 version of the 6 PM surge profile. IMPLEMENTATION.md §14, PRD §8.2.
//
//   k6 run -e BASE_URL=http://localhost:8080 test/load/race.js
//
// `make load` does NOT use this file. The Go driver next to it is the one that
// runs, deliberately: k6 is a host dependency, this project's whole demo posture
// is that nothing at the venue needs installing or downloading, and a load gate
// that cannot run on the machine in front of you is a gate in name only.
//
// This exists for the case where k6 IS available and you want its output — the
// thresholds below are the same three, expressed in k6's vocabulary, so the two
// drivers cannot quietly disagree about what passing means.
//
// Prerequisites: a running API in AUTH_MODE=dev (`make run`) and a seeded
// database (`make seed`), because it authenticates through POST /api/v1/dev/login.
//
// Caveat worth knowing before you read the numbers: this logs in as the ten
// seeded students, so 500 VUs are 50 requests per account. The fair-use cap will
// refuse some of them with a 422 that the Go driver never sees, because that one
// mints a distinct throwaway user per VU. Treat this as the latency cross-check,
// not as the correctness gate.

import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';
import { uuidv4 } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const FACILITY_ID = __ENV.FACILITY_ID; // required: the contended court
const START = __ENV.START;             // required: RFC3339, UTC
const DURATION_MINUTES = Number(__ENV.DURATION_MINUTES || 60);
const VUS = Number(__ENV.VUS || 500);

// Separate trends per outcome. A single latency distribution would average the
// winner, the losers and the shed together and report a number that describes
// nobody — the entire point of §14's split is that these are three different
// populations with three different budgets.
const confirmed = new Trend('booking_confirmed_ms', true);
const conflict = new Trend('booking_conflict_ms', true);
const shed = new Trend('booking_shed_ms', true);

export const options = {
  scenarios: {
    surge: {
      // Every VU once, together. Not a ramp: the event being reproduced is a
      // few hundred students hitting one slot in the same second, and a ramp
      // would spread them out until there is no race left to measure.
      executor: 'shared-iterations',
      vus: VUS,
      iterations: VUS,
      maxDuration: '60s',
    },
  },
  thresholds: {
    // These FAIL the run. Non-negotiable #6: losing must be faster than winning,
    // so the rejection budget is the tighter of the two.
    'booking_conflict_ms': ['p(99)<150'],
    'booking_confirmed_ms': ['p(99)<250'],
    // A 500 is the system not knowing what happened. There is no acceptable rate.
    'http_req_failed{expected_response:true}': ['rate<0.01'],
    'checks{check:no 5xx}': ['rate==1.00'],
  },
};

export function setup() {
  if (!FACILITY_ID || !START) {
    throw new Error('set FACILITY_ID and START (RFC3339). `make load` needs neither — it resolves both itself.');
  }

  const tokens = [];
  for (let i = 1; i <= 10; i++) {
    const rollNo = 'student' + String(i).padStart(2, '0');
    const res = http.post(`${BASE_URL}/api/v1/dev/login`, JSON.stringify({ roll_no: rollNo }), {
      headers: { 'Content-Type': 'application/json' },
    });
    if (res.status !== 200) {
      throw new Error(`dev login failed for ${rollNo}: ${res.status} ${res.body}`);
    }
    tokens.push(res.json('token'));
  }
  return { tokens };
}

export default function (data) {
  const token = data.tokens[__VU % data.tokens.length];

  const res = http.post(
    `${BASE_URL}/api/v1/bookings`,
    JSON.stringify({
      facility_id: FACILITY_ID,
      start: START,
      duration_minutes: DURATION_MINUTES,
    }),
    {
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${token}`,
        // A fresh key per attempt. Reusing one turns the second request into an
        // idempotent replay — a 200, not a race.
        'Idempotency-Key': uuidv4(),
      },
      // 409 and 429 are correct answers here, not failures. Declaring them
      // expected is what keeps http_req_failed meaningful: without this, a
      // perfect run reads as 99.8% failure.
      responseCallback: http.expectedStatuses(200, 201, 409, 429),
      tags: { name: 'POST /api/v1/bookings' },
    }
  );

  check(res, { 'no 5xx': (r) => r.status < 500 });

  if (res.status === 201 || res.status === 200) confirmed.add(res.timings.duration);
  else if (res.status === 409) conflict.add(res.timings.duration);
  else if (res.status === 429) shed.add(res.timings.duration);
}

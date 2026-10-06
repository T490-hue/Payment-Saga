import http from "k6/http";
import { check } from "k6";

// 50% of requests are retries of the same idempotency key —
// these hit the cache path and show the biggest latency difference
// between redis and postgres modes.

const BASE = __ENV.BASE_URL || "http://localhost:8090";
const SHARED_KEY = "bench-retry-key-001";

export default function () {
  const isRetry = Math.random() < 0.5;
  const key = isRetry ? SHARED_KEY : `bench-${Date.now()}-${Math.random()}`;

  const res = http.post(
    `${BASE}/v1/transfers`,
    JSON.stringify({
      from_account_id: "alice",
      to_account_id: "bob",
      amount_cents: 100,
      currency: "USD",
    }),
    {
      headers: {
        "Content-Type": "application/json",
        "Idempotency-Key": key,
      },
    }
  );

  check(res, {
    "success or idempotent replay": (r) =>
      r.status === 200 || r.status === 201,
  });
}

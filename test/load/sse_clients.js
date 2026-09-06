// Many concurrent SSE clients (PART 154): each VU holds /events/stream open and
// counts frames. Measures connection fan-out and that heartbeats keep
// connections alive behind proxies.
//
// WHAT k6 CAN AND CANNOT SEE HERE. k6 has no streaming HTTP client: it issues a
// normal GET and aborts it with `timeout`. For a response that never completes,
// it hands back the bytes received so far but reports `status: 0`, `error:
// "request timeout"` and an EMPTY header map. Verified directly: a 20s request
// against a live stream returns `status=0 header_count=0 body_len=13` — the
// heartbeat arrived, the headers did not.
//
// So this file must not assert on status or headers. An earlier version checked
// `Content-Type` starts with `text/event-stream`; that check failed 600 out of
// 600 times against an endpoint that demonstrably returns exactly that header,
// because k6 never exposed it. A check that cannot pass is as useless as one
// that cannot fail.
//
// The response headers ARE asserted, over a real connection, by the Go suite:
// `TestEventStream_RealStreamPackageOverARealConnection` in internal/httpapi
// checks `text/event-stream`, `no-cache` and `X-Accel-Buffering`, that a
// published event and a heartbeat both arrive, and that shutdown ends the
// connection and frees the client. That is the right split: Go proves the
// protocol, k6 measures fan-out under load.
import http from "k6/http";
import { check } from "k6";
import { Rate } from "k6/metrics";
import { BASE_URL, params } from "./lib.js";

// Tracked separately so a regression shows up as "clients that received
// nothing" rather than being averaged into the generic checks rate.
const framesReceived = new Rate("sse_frames_received");

export const options = {
  scenarios: { streams: { executor: "constant-vus", vus: 200, duration: "1m" } },
  thresholds: {
    checks: ["rate>0.99"],
    // Every held connection must produce at least one frame. A stream that
    // accepts a client and then goes silent looks healthy to a load balancer
    // and is useless to a browser.
    "sse_frames_received": ["rate>0.99"],
  },
};

export default function () {
  const res = http.get(
    `${BASE_URL}/events/stream`,
    params({ timeout: "20s", headers: { Accept: "text/event-stream" }, tags: { name: "sse" } }),
  );

  // The body is the only observable: SSE comment frames (": keepalive") or
  // named events ("event:"). Anything else means the connection was accepted
  // and produced no traffic.
  const body = typeof res.body === "string" ? res.body : "";
  const gotFrame = body.includes(": keepalive") || body.includes("event:");

  framesReceived.add(gotFrame);
  check(res, {
    "received a keepalive or event": () => gotFrame,
    // A refusal completes immediately and therefore DOES carry a status, so
    // this catches auth or routing regressions without depending on headers.
    "was not refused": (r) => r.status === 0 || (r.status >= 200 && r.status < 300),
  });
}

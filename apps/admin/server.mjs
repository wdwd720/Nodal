// Development server for the operator console: static files from dist/ plus a
// same-origin reverse proxy to the API.
//
// The proxy is not a convenience. The session cookie is SameSite=Lax and the
// CSRF guard is Fetch Metadata (internal/auth/httpmw/csrf.go), so a console
// served from a different origin than the API would send no cookie at all on
// navigation and be refused "cross-site request refused" on every command.
// Serving both through one origin is what a production deployment does too;
// this reproduces it locally instead of loosening either control.
//
//   node server.mjs
//   ADMIN_PORT=5174 API_ORIGIN=http://127.0.0.1:8080 node server.mjs
//
// It is a development tool. It binds loopback only, adds no authentication of
// its own, and must never be used to serve the console in a deployed
// environment.
import http from "node:http";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const distDir = path.join(here, "dist");
const port = Number(process.env.ADMIN_PORT ?? 5174);
const host = process.env.ADMIN_HOST ?? "127.0.0.1";
const apiOrigin = process.env.API_ORIGIN ?? "http://127.0.0.1:8080";

/** Path prefixes served by the API rather than by this process. */
const proxiedPrefixes = ["/v1/", "/auth/"];

const contentTypes = new Map(
  Object.entries({
    ".html": "text/html; charset=utf-8",
    ".js": "text/javascript; charset=utf-8",
    ".mjs": "text/javascript; charset=utf-8",
    ".css": "text/css; charset=utf-8",
    ".json": "application/json; charset=utf-8",
    ".svg": "image/svg+xml",
    ".ico": "image/x-icon",
    ".woff2": "font/woff2",
  }),
);

if (!fs.existsSync(distDir)) {
  console.error("admin: dist/ is missing. Run `node build.mjs` first.");
  process.exit(1);
}

function isProxied(pathname) {
  return proxiedPrefixes.some((p) => pathname === p.slice(0, -1) || pathname.startsWith(p));
}

async function proxy(req, res, url) {
  const target = new URL(url.pathname + url.search, apiOrigin);
  const headers = new Headers();
  for (const [k, v] of Object.entries(req.headers)) {
    if (v === undefined) continue;
    const key = k.toLowerCase();
    // Hop-by-hop headers and the ones the fetch layer must own itself.
    if (["host", "connection", "content-length", "accept-encoding"].includes(key)) continue;
    headers.set(k, Array.isArray(v) ? v.join(", ") : v);
  }
  // The console is same-origin with the API from the browser's point of view;
  // say so accurately rather than letting the guard see a missing header.
  if (!headers.has("sec-fetch-site")) headers.set("Sec-Fetch-Site", "same-origin");

  let body;
  if (req.method !== "GET" && req.method !== "HEAD") {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    body = Buffer.concat(chunks);
  }

  let upstream;
  try {
    upstream = await fetch(target, { method: req.method, headers, body, redirect: "manual" });
  } catch (err) {
    res.writeHead(502, { "content-type": "application/problem+json" });
    res.end(
      JSON.stringify({
        type: "urn:problem:provider-unavailable",
        title: "The API is not reachable",
        status: 502,
        code: "PROVIDER_UNAVAILABLE",
        detail: `${apiOrigin} did not answer: ${err instanceof Error ? err.message : String(err)}`,
      }),
    );
    return;
  }

  const out = {};
  upstream.headers.forEach((value, key) => {
    if (key === "content-encoding" || key === "content-length" || key === "transfer-encoding") return;
    if (key === "location") {
      // Keep redirects on this origin so the dev login flow stays same-origin.
      out[key] = value.startsWith(apiOrigin) ? value.slice(apiOrigin.length) || "/" : value;
      return;
    }
    out[key] = value;
  });
  const setCookie = upstream.headers.getSetCookie?.() ?? [];
  if (setCookie.length > 0) out["set-cookie"] = setCookie;

  res.writeHead(upstream.status, out);
  if (req.method === "HEAD") {
    res.end();
    return;
  }
  res.end(Buffer.from(await upstream.arrayBuffer()));
}

function serveStatic(req, res, url) {
  const rel = url.pathname === "/" ? "/index.html" : url.pathname;
  const resolved = path.join(distDir, path.normalize(rel).replace(/^([/\\])+/, ""));
  if (!resolved.startsWith(distDir)) {
    res.writeHead(403).end("forbidden");
    return;
  }
  let file = resolved;
  if (!fs.existsSync(file) || fs.statSync(file).isDirectory()) {
    // Single-page app: unknown paths render the shell, which routes them.
    file = path.join(distDir, "index.html");
  }
  if (!fs.existsSync(file)) {
    res.writeHead(404).end("not found");
    return;
  }
  const type = contentTypes.get(path.extname(file)) ?? "application/octet-stream";
  res.writeHead(200, { "content-type": type, "cache-control": "no-store" });
  fs.createReadStream(file).pipe(res);
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url ?? "/", `http://${host}:${port}`);
  if (isProxied(url.pathname)) {
    proxy(req, res, url).catch((err) => {
      console.error("admin: proxy error:", err);
      if (!res.headersSent) res.writeHead(500);
      res.end();
    });
    return;
  }
  serveStatic(req, res, url);
});

server.listen(port, host, () => {
  console.log(`admin console  http://${host}:${port}`);
  console.log(`api proxied to ${apiOrigin} for /v1 and /auth`);
  console.log(`sign in        http://${host}:${port}/v1/auth/login`);
});

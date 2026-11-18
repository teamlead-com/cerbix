// The phone provider stub of the dev stack (FR-041a, e2e/tests/phone-alerts.spec.ts). It stands where
// a generic HTTP SMS gateway would: cerbix's telephony settings point at http://phonestub:8026/sms,
// and every request it receives is RECORDED and answered — never forwarded. No real provider is ever
// called from the dev stack.
//
// Dependency-free Node 22; run by the stock node:22-alpine image (docker/docker-compose.yml, profile
// "phone"). Dev stacks only.
//
//   POST|PUT  <any path outside /_stub/>  recorded; answers 201 {"sid":"STUB<seq>"} or the next
//                                         programmed answer
//   GET       /_stub/messages[?to=E.164]  the recorded list, oldest first (optionally one recipient)
//   DELETE    /_stub/messages             clears the list and the programmed queue
//   POST      /_stub/next {status, body}  queues one programmed answer
//   GET       /_stub/health               200
//
// Header VALUES are never stored, only their names: a header can carry a gateway credential, and the
// E2E spec checks that the one it sets reaches the stub without the stub keeping it.

import http from "node:http";

const PORT = Number(process.env.PORT || 8026);
const KEEP = 500;
const MAX_BODY = 64 * 1024;

let seq = 0;
let messages = [];
let queue = [];

function send(res, status, body) {
  const text = body === undefined ? "" : typeof body === "string" ? body : JSON.stringify(body);
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(text);
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on("data", (c) => {
      size += c.length;
      if (size > MAX_BODY) {
        reject(new Error("body too large"));
        req.destroy();
        return;
      }
      chunks.push(c);
    });
    req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
    req.on("error", reject);
  });
}

// to/text from a JSON object or a form body — the keys the spec's body template uses.
function fields(contentType, body) {
  const ct = String(contentType || "").toLowerCase();
  try {
    if (ct.startsWith("application/json")) {
      const doc = JSON.parse(body);
      if (doc && typeof doc === "object") return { to: str(doc.to), text: str(doc.text) };
    } else if (ct.startsWith("application/x-www-form-urlencoded")) {
      const form = new URLSearchParams(body);
      return { to: str(form.get("to")), text: str(form.get("text")) };
    }
  } catch {
    /* recorded with to/text null */
  }
  return { to: null, text: null };
}

function str(v) {
  return typeof v === "string" ? v : v == null ? null : String(v);
}

// A "+" in a query string decodes to a space when the caller forgot to encode it.
function normalizeTo(v) {
  if (v == null) return null;
  const t = v.trim();
  return /^\d/.test(t) && v.startsWith(" ") ? "+" + t : t;
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url || "/", "http://stub");
  const path = url.pathname;
  try {
    if (path === "/_stub/health" && req.method === "GET") return send(res, 200, { ok: true });
    if (path === "/_stub/messages" && req.method === "GET") {
      const to = normalizeTo(url.searchParams.get("to"));
      return send(res, 200, to ? messages.filter((m) => m.to === to) : messages);
    }
    if (path === "/_stub/messages" && req.method === "DELETE") {
      messages = [];
      queue = [];
      return send(res, 204);
    }
    if (path === "/_stub/next" && req.method === "POST") {
      const doc = JSON.parse((await readBody(req)) || "{}");
      const status = Number(doc.status);
      if (!Number.isInteger(status) || status < 200 || status > 599) return send(res, 400, { error: "status must be 200..599" });
      queue.push({ status, body: doc.body === undefined ? {} : doc.body });
      return send(res, 204);
    }
    if (path.startsWith("/_stub/")) return send(res, 404, { error: "unknown stub route" });

    if (req.method !== "POST" && req.method !== "PUT") return send(res, 405, { error: "POST or PUT" });
    const body = await readBody(req);
    const contentType = req.headers["content-type"] || "";
    const { to, text } = fields(contentType, body);
    seq += 1;
    messages.push({
      seq,
      at: new Date().toISOString(),
      method: req.method,
      path,
      content_type: contentType,
      header_names: Object.keys(req.headers).sort(),
      to,
      text,
      body,
    });
    if (messages.length > KEEP) messages = messages.slice(messages.length - KEEP);
    const programmed = queue.shift();
    if (programmed) {
      if (programmed.status >= 300 && programmed.status < 400) {
        res.writeHead(programmed.status, { Location: "http://phonestub.invalid/elsewhere" });
        return res.end();
      }
      return send(res, programmed.status, programmed.body);
    }
    return send(res, 201, { sid: `STUB${seq}` });
  } catch (err) {
    return send(res, 400, { error: String(err && err.message ? err.message : err) });
  }
});

server.listen(PORT, "0.0.0.0", () => {
  console.log(`phone stub listening on :${PORT}`);
});

for (const sig of ["SIGINT", "SIGTERM"]) process.on(sig, () => server.close(() => process.exit(0)));

// Dev mock of the pos-ticker admin API (NOT embedded). No dependencies.
//   node web/dev/mock-server.mjs            -> http://127.0.0.1:8000/admin
//   MOCK_KEY=... PORT=... node web/dev/mock-server.mjs
// Serves the UI from web/ and fakes every endpoint of the admin API,
// including SSE (/events) and /preview (1x1 PNG). Sends the contract CSP so
// violations show up in the browser console.
import http from "node:http";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import crypto from "node:crypto";

const here = path.dirname(fileURLToPath(import.meta.url));
const webDir = path.resolve(here, "..");
const PORT = +(process.env.PORT || 8000);
const KEY = process.env.MOCK_KEY || "dev-admin-key-123456";
const PNG = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==", "base64");
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob: data:; connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'";

const sessions = new Set();
const sse = new Set();
const settings = {
  prints_per_hour: "10", max_per_ip_hour: "3", line_width: "32", allowed_origin: "https://your-domain.com",
  printer_mode: "auto", printer_host: "", printer_port: "9100", email_notifications: "true",
  receipt_font: "receipt", receipt_font_size: "22", receipt_show_timestamp: "true", receipt_show_email: "true",
  receipt_show_id: "true", receipt_title: "", receipt_footer: "",
};
const INT_KEYS = ["prints_per_hour", "max_per_ip_hour", "line_width", "printer_port", "receipt_font_size"];
const nowUTC = () => new Date().toISOString().slice(0, 19).replace("T", " ");
let nextId = 1;
const msgs = [];
const names = ["Ada Lovelace", "Grace Hopper", "Linus T.", "Margaret H.", "Alan Turing", "Katherine J."];
function addMsg(over = {}) {
  const id = nextId++;
  const m = {
    id, name: names[id % names.length], email: id % 3 ? `user${id}@example.com` : "",
    message: `Test message number ${id}. ` + "Lorem ipsum dolor sit amet. ".repeat(1 + (id % 4)),
    ip: "203.0.113." + (id % 250), received_at: nowUTC(), browser_time: id % 2 ? "Mon  May 12  2:41 PM" : null,
    printed_at: null, headers: null, claimed: 0, has_image: id % 5 === 0, ...over,
  };
  msgs.push(m);
  return m;
}
for (let i = 0; i < 40; i++) addMsg();
msgs[0].printed_at = nowUTC(); msgs[1].claimed = 2; msgs[2].claimed = 1;
let printer = { up: true, mode: "network:192.168.1.50:9100", last_error: "" };

const counts = () => {
  const pending = msgs.filter((m) => !m.printed_at && m.claimed === 0).length;
  return { pending, total: msgs.length, printed_this_hour: msgs.filter((m) => m.printed_at).length,
    in_flight: msgs.filter((m) => m.claimed === 1 && !m.printed_at).length, limit_per_hour: +settings.prints_per_hour };
};
const broadcast = (type, data) => { for (const r of sse) r.write(`event: ${type}\ndata: ${JSON.stringify(data)}\n\n`); };
const status = () => ({ ...counts(), printer });

function simulatePrint(m) {
  setTimeout(() => {
    const ok = m.id % 7 !== 0;
    if (ok) { m.printed_at = nowUTC(); m.claimed = 0; printer = { ...printer, up: true, last_error: "" }; }
    else { m.claimed = 0; printer = { ...printer, up: false, last_error: "write tcp: broken pipe" }; }
    broadcast("print", ok ? { id: m.id, ok: true } : { id: m.id, ok: false, error: "write tcp: broken pipe" });
    broadcast("status", status());
  }, 1200);
}

const send = (res, code, obj) => {
  res.writeHead(code, { "Content-Type": "application/json" });
  res.end(JSON.stringify(obj));
};
const readBody = (req) => new Promise((ok) => {
  const chunks = [];
  req.on("data", (c) => chunks.push(c));
  req.on("end", () => { try { ok(JSON.parse(Buffer.concat(chunks).toString() || "null")); } catch { ok(null); } });
});
const cookieOf = (req) => (req.headers.cookie || "").split(/;\s*/).find((c) => c.startsWith("pt_session="))?.slice(11);

const server = http.createServer(async (req, res) => {
  res.setHeader("Content-Security-Policy", CSP);
  res.setHeader("X-Content-Type-Options", "nosniff");
  res.setHeader("X-Frame-Options", "DENY");
  res.setHeader("Referrer-Policy", "no-referrer");
  const url = new URL(req.url, "http://x");
  const p = url.pathname, method = req.method;

  if (method === "GET" && p === "/") { res.writeHead(302, { Location: "/admin" }); return res.end(); }
  if (method === "GET" && (p === "/admin" || p === "/admin/")) {
    res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
    return res.end(fs.readFileSync(path.join(webDir, "index.html")));
  }
  if (method === "GET" && p.startsWith("/assets/")) {
    const f = path.basename(p);
    if (!["app.js", "app.css"].includes(f)) { res.writeHead(404); return res.end(); }
    res.writeHead(200, { "Content-Type": f.endsWith(".js") ? "text/javascript" : "text/css", "Cache-Control": "no-store" });
    return res.end(fs.readFileSync(path.join(webDir, f)));
  }
  if (method === "GET" && p === "/health") return send(res, 200, { ok: true, queue_depth: counts().in_flight, printer: printer.mode, db: "ok" });

  if (method === "POST" && p === "/login") {
    if (req.headers["x-requested-with"] !== "pos-ticker") return send(res, 403, { error: "csrf" });
    const b = await readBody(req);
    if (!b || b.key !== KEY) return send(res, 401, { error: "invalid key" });
    const tok = crypto.randomBytes(32).toString("hex");
    sessions.add(tok);
    res.setHeader("Set-Cookie", `pt_session=${tok}; HttpOnly; SameSite=Strict; Path=/; Max-Age=43200`);
    return send(res, 200, { ok: true });
  }

  // everything below needs auth
  const authed = sessions.has(cookieOf(req)) || req.headers["x-admin-key"] === KEY;
  if (!authed) return send(res, 401, { error: "unauthorized" });
  if (method !== "GET" && req.headers["x-admin-key"] !== KEY && req.headers["x-requested-with"] !== "pos-ticker")
    return send(res, 403, { error: "missing X-Requested-With" });

  if (method === "POST" && p === "/logout") {
    sessions.delete(cookieOf(req));
    res.setHeader("Set-Cookie", "pt_session=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0");
    return send(res, 200, { ok: true });
  }
  if (method === "GET" && p === "/events") {
    res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
    res.write(`event: status\ndata: ${JSON.stringify(status())}\n\n`);
    sse.add(res);
    req.on("close", () => sse.delete(res));
    return;
  }
  if (method === "GET" && p === "/queue") return send(res, 200, counts());
  if (method === "POST" && p === "/drain") {
    const pend = msgs.filter((m) => !m.printed_at && m.claimed === 0);
    pend.forEach((m) => { m.claimed = 1; simulatePrint(m); });
    broadcast("status", status());
    return send(res, 200, { queued_to_print: pend.length });
  }
  if (method === "POST" && p === "/print/image") {
    const b = await readBody(req);
    if (!b || !["bitmap", "ascii"].includes(b.mode)) return send(res, 400, { error: "mode must be 'bitmap' or 'ascii'" });
    if (!b.image_b64) return send(res, 400, { error: "invalid base64" });
    return send(res, 200, { queued: true, mode: b.mode });
  }
  let m;
  if (method === "POST" && (m = p.match(/^\/(print|skip)\/(\d+)$/))) {
    const msg = msgs.find((x) => x.id === +m[2]);
    if (!msg || msg.printed_at || msg.claimed !== 0) return send(res, 404, { error: "message not found or already printed/claimed" });
    if (m[1] === "print") { msg.claimed = 1; simulatePrint(msg); broadcast("status", status()); return send(res, 200, { queued: true }); }
    msg.claimed = 2; broadcast("status", status());
    return send(res, 200, { skipped: true });
  }
  if (method === "GET" && p === "/messages") {
    const limit = +(url.searchParams.get("limit") || 50), offset = +(url.searchParams.get("offset") || 0);
    const q = (url.searchParams.get("q") || "").toLowerCase();
    const all = msgs.filter((x) => !q || `${x.name} ${x.email} ${x.message}`.toLowerCase().includes(q)).reverse();
    return send(res, 200, { total: all.length, messages: all.slice(offset, offset + limit) });
  }
  if (method === "GET" && (m = p.match(/^\/messages\/(\d+)\/image$/))) {
    const msg = msgs.find((x) => x.id === +m[1]);
    if (!msg || !msg.has_image) return send(res, 404, { error: "no image" });
    res.writeHead(200, { "Content-Type": "image/png" });
    return res.end(PNG);
  }
  if (method === "GET" && p === "/settings") return send(res, 200, settings);
  if (method === "PATCH" && p === "/settings") {
    const b = await readBody(req);
    const kv = b && b.settings;
    if (!kv || typeof kv !== "object") return send(res, 400, { error: "bad body" });
    const unknown = Object.keys(kv).filter((k) => !(k in settings));
    if (unknown.length) return send(res, 400, { error: `Unknown settings: ${unknown}` });
    for (const k of Object.keys(kv)) if (INT_KEYS.includes(k) && !/^-?\d+$/.test(String(kv[k]))) return send(res, 400, { error: `Setting '${k}' must be an integer` });
    Object.assign(settings, Object.fromEntries(Object.entries(kv).map(([k, v]) => [k, String(v)])));
    broadcast("settings", settings);
    return send(res, 200, settings);
  }
  if (method === "POST" && p === "/preview") {
    const b = await readBody(req);
    if (!b || (b.kind === "message" && !String(b.message || "").trim())) return send(res, 400, { error: "message required" });
    if (b.kind === "image" && !b.image_b64) return send(res, 400, { error: "image_b64 required" });
    if (!["message", "image"].includes(b.kind)) return send(res, 400, { error: "bad kind" });
    res.writeHead(200, { "Content-Type": "image/png" });
    return res.end(PNG);
  }
  send(res, 404, { error: "not found" });
});

// Simulate a new public submission every 25s so live updates are visible.
setInterval(() => {
  const m = addMsg();
  broadcast("message", m);
  broadcast("status", status());
}, 25000).unref();

server.listen(PORT, "127.0.0.1", () => console.log(`mock admin: http://127.0.0.1:${PORT}/admin  (key: ${KEY})`));
export { server };

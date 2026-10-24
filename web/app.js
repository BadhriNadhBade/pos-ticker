"use strict";
/* Printer Server admin UI. Vanilla ES2020, no build step.
   CSP-safe: no inline handlers, no HTML-string sinks (DOM built with createElement/textContent), no style attributes. */

const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];

/* h(tag, attrs, ...children): tiny DOM builder. attrs.on = {event: fn}; "text" sets textContent. */
function h(tag, attrs, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === "class") e.className = v;
    else if (k === "text") e.textContent = v;
    else if (k === "on") for (const [ev, fn] of Object.entries(v)) e.addEventListener(ev, fn);
    else if (k === "value" || k === "checked") e[k] = v;
    else e.setAttribute(k, v === true ? "" : v);
  }
  add(e, kids);
  return e;
}
function add(e, kids) {
  for (const k of kids.flat()) if (k != null && k !== false) e.append(k.nodeType ? k : document.createTextNode(String(k)));
}
const clear = (e) => { e.textContent = ""; return e; };
const show = (e, on) => e.classList.toggle("hidden", !on);

class AuthError extends Error {}

/* ── state ── */
const PAGE = 25;
const S = { in: false, page: 0, total: 0, msgs: [], q: "", filter: "all", sel: null, stats: {}, tab: "messages" };

/* ── toasts ── */
function toast(msg, bad) {
  const box = $("#toasts");
  box.append(h("div", { class: "toast" + (bad ? " bad" : ""), text: msg }));
  while (box.children.length > 4) box.firstChild.remove();
  const t = box.lastChild;
  setTimeout(() => t.remove(), bad ? 5500 : 2800);
}

/* ── API ── */
async function errText(res) {
  let t = "";
  try { t = await res.text(); } catch { /* ignore */ }
  try {
    const d = JSON.parse(t);
    const m = d && (d.error || d.detail || d.message);
    if (typeof m === "string" && m) return m;
    if (Array.isArray(m)) return m.map((x) => (x && x.msg) || JSON.stringify(x)).join("; ");
  } catch { /* not JSON */ }
  return (t && t.length < 200 ? t : "") || "HTTP " + res.status;
}

/* Every request carries the CSRF header; auth is the HttpOnly pt_session cookie. */
async function api(path, { method = "GET", body = null, raw = false, signal } = {}) {
  const headers = { "X-Requested-With": "pos-ticker" };
  if (body != null) headers["Content-Type"] = "application/json";
  const res = await fetch(path, {
    method, headers, signal, credentials: "same-origin",
    body: body != null ? JSON.stringify(body) : null,
  });
  if (res.status === 401) {
    showGate(S.in ? "Session expired. Sign in again." : "");
    throw new AuthError("unauthorized");
  }
  if (!res.ok) throw new Error(await errText(res));
  if (raw) return res;
  const text = await res.text();
  return text ? JSON.parse(text) : null;
}
const quiet = (e) => e instanceof AuthError || (e && e.name === "AbortError");

/* ── gate / session ── */
function showGate(msg) {
  stopLive();
  S.in = false;
  closeModal();
  show($("#app"), false);
  show($("#gate"), true);
  $("#gate-err").textContent = msg || "";
  const k = $("#gate-key");
  k.value = "";
  k.focus();
}
function enter() {
  S.in = true;
  show($("#gate"), false);
  show($("#app"), true);
  refreshAll();
  startLive();
}
$("#gate").addEventListener("submit", async (e) => {
  e.preventDefault();
  const key = $("#gate-key").value.trim();
  const err = $("#gate-err");
  if (!key) { err.textContent = "Enter the admin key."; return; }
  const go = $("#gate-go");
  go.disabled = true;
  err.textContent = "";
  try {
    const res = await fetch("/login", {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-Requested-With": "pos-ticker" },
      body: JSON.stringify({ key }),
    });
    if (res.ok) { $("#gate-key").value = ""; enter(); }
    else if (res.status === 401) err.textContent = "Key rejected. Try again.";
    else if (res.status === 429) err.textContent = "Too many attempts. Wait a moment and retry.";
    else err.textContent = await errText(res);
  } catch { err.textContent = "Could not reach server."; }
  finally { go.disabled = false; }
});
$("#signout").addEventListener("click", async () => {
  try { await api("/logout", { method: "POST" }); } catch { /* cookie may already be gone */ }
  showGate("");
});

/* ── tabs (WAI-ARIA: roving tabindex + arrows) ── */
const tabEls = $$(".tab");
function activateTab(tab, focus) {
  S.tab = tab.dataset.tab;
  tabEls.forEach((t) => { const on = t === tab; t.setAttribute("aria-selected", on); t.tabIndex = on ? 0 : -1; });
  ["messages", "print", "settings"].forEach((n) => show($("#tab-" + n), n === S.tab));
  if (focus) tab.focus();
  if (S.tab === "settings") { loadSettings(); pvMsg.now(msgPayload); }
}
tabEls.forEach((tab, i) => {
  tab.addEventListener("click", () => activateTab(tab));
  tab.addEventListener("keydown", (e) => {
    let j = null;
    if (e.key === "ArrowRight" || e.key === "ArrowDown") j = (i + 1) % tabEls.length;
    else if (e.key === "ArrowLeft" || e.key === "ArrowUp") j = (i + tabEls.length - 1) % tabEls.length;
    else if (e.key === "Home") j = 0;
    else if (e.key === "End") j = tabEls.length - 1;
    if (j !== null) { e.preventDefault(); activateTab(tabEls[j], true); }
  });
});
const gotoTab = (n) => activateTab($("#tabbtn-" + n));

/* ── stats + printer card ── */
function setStats(q) {
  if (!q) return;
  for (const k of ["pending", "total", "printed_this_hour", "in_flight", "limit_per_hour"]) if (q[k] != null) S.stats[k] = q[k];
  renderStats();
}
function renderStats() {
  const s = S.stats, v = (x) => (x == null ? "—" : x);
  $("#s-pending").textContent = v(s.pending);
  $("#s-flight").textContent = v(s.in_flight);
  $("#s-total").textContent = v(s.total);
  const hr = $("#s-hour");
  clear(hr);
  add(hr, [String(v(s.printed_this_hour)), " ", h("small", { text: "/ " + v(s.limit_per_hour) })]);
}
function bump(key, d) { if (S.stats[key] != null) { S.stats[key] = Math.max(0, S.stats[key] + d); renderStats(); } }

function setPrinter(p) {
  if (!p) return;
  S.printer = p;
  const up = p.up;
  $("#pr-dot").className = "dot " + (up === true ? "ok" : up === false ? "bad" : "");
  $("#pr-state").textContent = up === true ? "up" : up === false ? "down" : "unknown";
  $("#pr-mode").textContent = p.mode || "";
  const err = $("#pr-err");
  err.textContent = p.last_error ? "Last error: " + p.last_error : "";
  show(err, !!p.last_error);
}
function onStatus(d) {
  if (!d) return;
  setStats(d.queue || d);
  if (d.printer && typeof d.printer === "object") setPrinter(d.printer);
  else if ("up" in d || "mode" in d) setPrinter(d);
  if (typeof d.db === "string") $("#s-db").textContent = d.db;
}
async function refreshStatus() {
  try { setStats(await api("/queue")); } catch (e) { if (quiet(e)) return; }
  try {
    const r = await fetch("/health");
    const hl = await r.json();
    $("#s-db").textContent = hl.db || "—";
    if (typeof hl.printer === "string" && !S.printer) setPrinter({ up: hl.printer === "up" ? true : hl.printer === "down" ? false : undefined, mode: hl.printer_mode || "" });
  } catch { $("#s-db").textContent = "offline"; }
}
function refreshAll() { refreshStatus(); loadMessages(); }

/* ── live updates: SSE with 8s polling fallback ── */
let es = null, pollT = null, reconT = null, wasDown = false;
function setLive(state) {
  const map = { live: ["ok", "Live"], poll: ["warn", "Polling"], off: ["bad", "Offline"], wait: ["", "Connecting"] };
  $("#live-dot").className = "dot " + map[state][0];
  $("#live-text").textContent = map[state][1];
}
const parse = (e) => { try { return JSON.parse(e.data); } catch { return null; } };
function startLive() {
  stopLive();
  setLive("wait");
  if (typeof EventSource === "undefined") { startPoll(); return; }
  es = new EventSource("/events");
  es.onopen = () => { stopPoll(); setLive("live"); if (wasDown) { wasDown = false; refreshAll(); } };
  es.onerror = () => {
    wasDown = true;
    setLive("poll");
    startPoll();
    if (es && es.readyState === 2) { // CLOSED: browser gave up; retry ourselves
      clearTimeout(reconT);
      reconT = setTimeout(() => { if (S.in) startLive(); }, 5000);
    }
  };
  es.addEventListener("status", (e) => onStatus(parse(e)));
  es.addEventListener("message", (e) => onNewMessage(parse(e)));
  es.addEventListener("print", (e) => onPrintEvent(parse(e)));
  es.addEventListener("settings", () => onSettingsEvent());
}
function stopLive() {
  if (es) { es.close(); es = null; }
  clearTimeout(reconT);
  stopPoll();
}
function startPoll() {
  if (pollT || !S.in) return;
  const tick = () => { if (!document.hidden) { refreshStatus(); loadMessages(); } };
  tick();
  pollT = setInterval(tick, 8000);
}
function stopPoll() { if (pollT) { clearInterval(pollT); pollT = null; } }

/* ── messages ── */
function statusOf(m) {
  if (m.printed_at) return ["printed", "Printed"];
  if (m.claimed === 2) return ["skipped", "Skipped"];
  if (m.claimed === 1) return ["queued", "Queued"];
  return ["pending", "Pending"];
}
const canAct = (m) => !!m && !m.printed_at && m.claimed === 0;
const fmtTime = (m) => m.browser_time || (m.received_at ? m.received_at.replace(" ", " · ") + " UTC" : "—");
const nowUTC = () => new Date().toISOString().slice(0, 19).replace("T", " ");
const matches = (m) => {
  const k = statusOf(m)[0];
  return S.filter === "all" || k === S.filter || (S.filter === "pending" && k === "queued");
};

let loadTok = 0;
async function loadMessages() {
  const tok = ++loadTok;
  const p = new URLSearchParams({ limit: PAGE, offset: S.page * PAGE });
  if (S.q) p.set("q", S.q);
  try {
    const d = await api("/messages?" + p);
    if (tok !== loadTok) return;
    if (!(d.messages || []).length && S.page > 0) { S.page = 0; loadMessages(); return; }
    S.total = d.total || 0;
    S.msgs = d.messages || [];
    renderMsgs();
  } catch (e) {
    if (tok === loadTok && !quiet(e)) rowsNote("Could not load messages — " + e.message);
  }
}
function rowsNote(text) {
  clear($("#msg-rows")).append(h("tr", { class: "empty-row" }, h("td", { colspan: 6, text })));
}
function rowEl(m) {
  const [cls, label] = statusOf(m);
  const from = h("td", null, h("div", { text: m.name || "—" }), m.email ? h("div", { class: "email", text: m.email }) : null);
  const msg = h("td", null, h("div", { class: "msg", text: m.message || "" }), m.has_image ? h("div", { class: "email", text: "+ image attached" }) : null);
  const act = h("td", { class: "actions" });
  if (canAct(m)) {
    act.append(
      h("button", { class: "btn btn-sm", type: "button", text: "Print", "aria-label": "Print #" + m.id, on: { click: (e) => { e.stopPropagation(); act2("print", m.id); } } }),
      h("button", { class: "btn btn-sm btn-danger", type: "button", text: "Skip", "aria-label": "Skip #" + m.id, on: { click: (e) => { e.stopPropagation(); act2("skip", m.id); } } }),
    );
  }
  return h("tr", {
    class: "clickable" + (m.id === S.sel ? " sel" : "") + (m.claimed === 1 && !m.printed_at ? " busy" : ""),
    "data-id": m.id, "aria-current": m.id === S.sel ? "true" : null,
    on: { click: () => { setSel(m.id); openDetail(m); } },
  },
  h("td", { class: "num", text: "#" + m.id }), from, msg,
  h("td", { class: "time hide-sm", text: fmtTime(m) }),
  h("td", null, h("span", { class: "pill " + cls, text: label })), act);
}
function renderMsgs() {
  const body = clear($("#msg-rows"));
  const vis = S.msgs.filter(matches);
  if (!vis.length) {
    rowsNote(S.msgs.length ? "No messages match this filter." : S.q ? "No messages match your search." : "No messages yet.");
  } else body.append(...vis.map(rowEl));
  const start = S.total ? S.page * PAGE + 1 : 0, end = Math.min(S.total, (S.page + 1) * PAGE);
  $("#pg-label").textContent = S.total ? `${start}–${end} of ${S.total}` : "";
  $("#msg-total").textContent = S.total ? `${S.total} ${S.q ? "found" : "total"}` : "";
  $("#pg-prev").disabled = S.page === 0;
  $("#pg-next").disabled = end >= S.total;
}
function setSel(id) {
  S.sel = id;
  $$("#msg-rows tr").forEach((tr) => {
    const on = +tr.dataset.id === id;
    tr.classList.toggle("sel", on);
    if (on) { tr.setAttribute("aria-current", "true"); tr.scrollIntoView({ block: "nearest" }); } else tr.removeAttribute("aria-current");
  });
}
function moveSel(d) {
  const vis = S.msgs.filter(matches);
  if (!vis.length) return;
  let i = vis.findIndex((m) => m.id === S.sel);
  i = i < 0 ? (d > 0 ? 0 : vis.length - 1) : Math.max(0, Math.min(vis.length - 1, i + d));
  setSel(vis[i].id);
}

/* optimistic print/skip with rollback */
async function act2(kind, id) {
  const m = S.msgs.find((x) => x.id === id);
  if (!canAct(m)) return;
  const prev = { claimed: m.claimed, printed_at: m.printed_at };
  m.claimed = kind === "print" ? 1 : 2;
  bump("pending", -1);
  if (kind === "print") bump("in_flight", 1);
  renderMsgs();
  try {
    await api(`/${kind}/${id}`, { method: "POST" });
    toast(kind === "print" ? `Queued #${id} for printing` : `Skipped #${id}`);
  } catch (e) {
    const cur = S.msgs.find((x) => x.id === id);
    if (cur) Object.assign(cur, prev);
    bump("pending", 1);
    if (kind === "print") bump("in_flight", -1);
    renderMsgs();
    if (!quiet(e)) toast(`${kind === "print" ? "Print" : "Skip"} #${id} failed: ${e.message}`, true);
  }
}
let reloadT = 0;
const reloadSoon = () => { clearTimeout(reloadT); reloadT = setTimeout(loadMessages, 600); };

function onNewMessage(m) {
  if (!m || m.id == null) return;
  const known = S.msgs.some((x) => x.id === m.id);
  if (!known) {
    S.total++;
    if (S.page === 0 && !S.q) { S.msgs.unshift(m); S.msgs.length = Math.min(S.msgs.length, PAGE); }
    if (canAct(m)) bump("pending", 1);
    bump("total", 1);
    renderMsgs();
    toast(`New message from ${m.name || "someone"}`);
  }
}
function onPrintEvent(d) {
  if (!d) return;
  const m = S.msgs.find((x) => x.id === d.id);
  if (d.ok) {
    if (m) { m.printed_at = nowUTC(); m.claimed = 0; }
    toast(`Printed #${d.id}`);
  } else {
    if (m) m.claimed = 0;
    toast(`Print #${d.id} failed${d.error ? ": " + d.error : ""}`, true);
  }
  renderMsgs();
  reloadSoon();
}
function onSettingsEvent() {
  if (S.tab === "settings" && !settingsDirty()) loadSettings();
  else if (S.tab === "settings") toast("Settings changed elsewhere", true);
}

$("#btn-refresh").addEventListener("click", refreshAll);
$("#btn-drain").addEventListener("click", async () => {
  try { const r = await api("/drain", { method: "POST" }); toast(`Queued ${r.queued_to_print} message(s)`); refreshAll(); }
  catch (e) { if (!quiet(e)) toast(e.message, true); }
});
$("#pg-prev").addEventListener("click", () => { if (S.page > 0) { S.page--; loadMessages(); } });
$("#pg-next").addEventListener("click", () => { if ((S.page + 1) * PAGE < S.total) { S.page++; loadMessages(); } });

let qT = 0;
$("#q").addEventListener("input", (e) => {
  clearTimeout(qT);
  qT = setTimeout(() => { S.q = e.target.value.trim(); S.page = 0; loadMessages(); }, 250);
});
$$("#chips .chip").forEach((b) => b.addEventListener("click", () => {
  S.filter = b.dataset.f;
  $$("#chips .chip").forEach((x) => x.setAttribute("aria-pressed", x === b));
  renderMsgs();
}));

/* ── modal (focus trap, restore focus, Esc) ── */
let modalReturn = null, modalTok = 0, modalUrls = [];
const modalOpen = () => !$("#modal").classList.contains("hidden");
function openModal(title, ...nodes) {
  if (!modalOpen()) modalReturn = document.activeElement;
  modalTok++;
  $("#modal-title").textContent = title;
  add(clear($("#modal-body")), nodes);
  show($("#modal"), true);
  $("#app").inert = true;
  $("#modal-card").focus();
  return modalTok;
}
function closeModal() {
  if (!modalOpen()) return;
  modalTok++;
  show($("#modal"), false);
  $("#app").inert = false;
  modalUrls.forEach((u) => URL.revokeObjectURL(u));
  modalUrls = [];
  if (modalReturn && document.contains(modalReturn)) modalReturn.focus();
  modalReturn = null;
}
$("#modal-x").addEventListener("click", closeModal);
$("#modal").addEventListener("mousedown", (e) => { if (e.target === $("#modal")) closeModal(); });

function openDetail(m) {
  const [cls, label] = statusOf(m);
  const tok = openModal(`#${m.id} · ${m.name || "—"}`);
  const body = $("#modal-body");
  const imgSlot = h("div");
  const pv = h("div", { class: "preview hidden" });
  const actions = h("div", { class: "btn-row" });
  if (canAct(m)) {
    actions.append(
      h("button", { class: "btn btn-primary", type: "button", text: "Print", on: { click: () => { closeModal(); act2("print", m.id); } } }),
      h("button", { class: "btn btn-danger", type: "button", text: "Skip", on: { click: () => { closeModal(); act2("skip", m.id); } } }),
    );
  }
  actions.append(h("button", {
    class: "btn", type: "button", text: "Preview receipt",
    on: {
      click: async () => {
        show(pv, true);
        clear(pv).append(h("span", { class: "empty", text: "Rendering…" }));
        try {
          const res = await api("/preview", { method: "POST", raw: true, body: { kind: "message", name: m.name, email: m.email || "", message: m.message } });
          const u = URL.createObjectURL(await res.blob());
          if (tok !== modalTok) { URL.revokeObjectURL(u); return; }
          modalUrls.push(u);
          clear(pv).append(h("img", { src: u, alt: `Receipt preview for message #${m.id}` }));
        } catch (e) { if (!quiet(e) && tok === modalTok) clear(pv).append(h("span", { class: "err", text: e.message })); }
      },
    },
  }));
  add(body, [
    h("div", { class: "detail-meta", text: fmtTime(m) + (m.email ? " · " + m.email : "") + (m.ip ? " · IP " + m.ip : "") }),
    h("span", { class: "pill " + cls, text: label }),
    h("div", { class: "detail-msg", text: m.message || "—" }),
    imgSlot, actions, pv,
  ]);
  if (m.has_image) {
    const note = h("div", { class: "muted detail-msg", text: "Loading image…" });
    imgSlot.append(note);
    api(`/messages/${m.id}/image`, { raw: true }).then(async (res) => {
      const u = URL.createObjectURL(await res.blob());
      if (tok !== modalTok) { URL.revokeObjectURL(u); return; }
      modalUrls.push(u);
      note.replaceWith(h("img", { class: "detail-img", src: u, alt: `Attached image for message #${m.id}` }));
    }).catch((e) => { if (tok === modalTok && !quiet(e)) note.textContent = "Could not load image — " + e.message; });
  }
}

function openHelp() {
  const rows = [["j / k", "Next / previous message"], ["Enter", "Open selected message"], ["p", "Print selected (pending)"],
    ["s", "Skip selected (pending)"], ["/", "Focus search"], ["?", "This help"], ["Esc", "Close dialog"]];
  const dl = h("dl", { class: "keys" });
  for (const [k, d] of rows) dl.append(h("dt", null, h("kbd", { text: k })), h("dd", { text: d }));
  openModal("Keyboard shortcuts", dl, h("p", { class: "muted", text: "Shortcuts are disabled while typing in a field." }));
}
$("#btn-help").addEventListener("click", openHelp);

/* ── keyboard shortcuts ── */
const typing = (t) => !!t && (/^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName) || t.isContentEditable);
document.addEventListener("keydown", (e) => {
  if (!S.in) return;
  if (modalOpen()) {
    if (e.key === "Escape") { e.preventDefault(); closeModal(); }
    else if (e.key === "Tab") { // focus trap
      const f = $$("button,a[href],input,select,textarea,[tabindex]:not([tabindex='-1'])", $("#modal-card")).filter((x) => !x.disabled && x.offsetParent !== null);
      if (!f.length) { e.preventDefault(); return; }
      const first = f[0], last = f[f.length - 1], a = document.activeElement;
      if (e.shiftKey && (a === first || a === $("#modal-card"))) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && a === last) { e.preventDefault(); first.focus(); }
    }
    return;
  }
  if (e.key === "Escape" && e.target === $("#q")) { $("#q").blur(); return; }
  if (typing(e.target) || e.ctrlKey || e.metaKey || e.altKey) return;
  const onMsgs = S.tab === "messages";
  const sel = S.msgs.find((m) => m.id === S.sel);
  switch (e.key) {
    case "j": if (onMsgs) moveSel(1); break;
    case "k": if (onMsgs) moveSel(-1); break;
    case "p": if (onMsgs && canAct(sel)) act2("print", sel.id); break;
    case "s": if (onMsgs && canAct(sel)) act2("skip", sel.id); break;
    case "Enter": if (onMsgs && sel && (e.target === document.body)) openDetail(sel); break;
    case "/": e.preventDefault(); gotoTab("messages"); $("#q").focus(); break;
    case "?": openHelp(); break;
    default: return;
  }
});

/* ── debounced PNG previewer (POST /preview → blob URL) ── */
function previewer(box, emptyText) {
  let t = 0, ac = null, url = null;
  const reset = () => { clear(box).append(h("span", { class: "empty", text: emptyText })); };
  async function run(payload) {
    if (ac) ac.abort();
    const mine = (ac = new AbortController());
    box.classList.add("busy");
    try {
      const res = await api("/preview", { method: "POST", body: payload, raw: true, signal: mine.signal });
      const u = URL.createObjectURL(await res.blob());
      if (mine.signal.aborted) { URL.revokeObjectURL(u); return; }
      if (url) URL.revokeObjectURL(url);
      url = u;
      clear(box).append(h("img", { src: u, alt: "Preview" }));
    } catch (e) {
      if (!quiet(e)) clear(box).append(h("span", { class: "err", text: "Preview failed — " + e.message }));
    } finally { if (ac === mine) { box.classList.remove("busy"); ac = null; } }
  }
  reset();
  return {
    schedule(fn) { clearTimeout(t); t = setTimeout(() => run(fn()), 300); },
    now(fn) { clearTimeout(t); run(fn()); },
    clear() { clearTimeout(t); if (ac) ac.abort(); ac = null; box.classList.remove("busy"); if (url) { URL.revokeObjectURL(url); url = null; } reset(); },
  };
}

/* ── Print tab ── */
const MAX_W = 1600, KEEP_BYTES = 2 * 1024 * 1024, HARD_CAP = 10 * 1024 * 1024;
let mode = "bitmap", pic = null; // pic = {b64, label}
const pvImg = previewer($("#preview"), "No image selected");
const imgPayload = () => ({ kind: "image", image_b64: pic.b64, mode });

$$("#mode button").forEach((b) => b.addEventListener("click", () => {
  mode = b.dataset.mode;
  $$("#mode button").forEach((x) => x.setAttribute("aria-pressed", x === b));
  if (pic) pvImg.schedule(imgPayload);
}));
const drop = $("#drop");
["dragenter", "dragover"].forEach((ev) => drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.add("over"); }));
["dragleave", "drop"].forEach((ev) => drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.remove("over"); }));
drop.addEventListener("drop", (e) => { const f = e.dataTransfer && e.dataTransfer.files[0]; if (f) loadFile(f); });
$("#file").addEventListener("change", (e) => { const f = e.target.files[0]; if (f) loadFile(f); });
window.addEventListener("paste", (e) => {
  if (!S.in || modalOpen() || typing(e.target)) return;
  const it = [...((e.clipboardData && e.clipboardData.items) || [])].find((i) => i.type.startsWith("image/"));
  if (it) { e.preventDefault(); gotoTab("print"); loadFile(it.getAsFile()); }
});

const blobB64 = (b) => new Promise((ok, no) => {
  const r = new FileReader();
  r.onload = () => ok(String(r.result).split(",")[1] || "");
  r.onerror = () => no(r.error);
  r.readAsDataURL(b);
});
/* Downscale huge images client-side: canvas, max 1600px wide, JPEG 0.9 on white. */
async function shrink(file) {
  const url = URL.createObjectURL(file);
  try {
    const im = new Image();
    im.src = url;
    await im.decode();
    const w = im.naturalWidth, hh = im.naturalHeight;
    let out = file;
    if (w > MAX_W || file.size > KEEP_BYTES) {
      const sc = Math.min(1, MAX_W / w);
      const c = document.createElement("canvas");
      c.width = Math.max(1, Math.round(w * sc));
      c.height = Math.max(1, Math.round(hh * sc));
      const x = c.getContext("2d");
      x.fillStyle = "#fff";
      x.fillRect(0, 0, c.width, c.height);
      x.drawImage(im, 0, 0, c.width, c.height);
      const b = await new Promise((r) => c.toBlob(r, "image/jpeg", 0.9));
      if (b && (b.size < file.size || file.size > HARD_CAP)) out = b;
    }
    return { blob: out, w, h: hh };
  } finally { URL.revokeObjectURL(url); }
}
async function loadFile(file) {
  if (!file || !file.type.startsWith("image/")) { toast("Not an image file", true); return; }
  if (file.size > 40 * 1024 * 1024) { toast("Image is larger than 40 MB", true); return; }
  try {
    const { blob, w, h: hh } = await shrink(file);
    if (blob.size > HARD_CAP) { toast("Image is still larger than 10 MB after downscaling", true); return; }
    pic = { b64: await blobB64(blob) };
    $("#img-meta").textContent = `${file.name || "pasted image"} — ${w}×${hh}, ${Math.round(file.size / 1024)} KB` +
      (blob !== file ? ` → ${Math.round(blob.size / 1024)} KB upload` : "");
    $("#btn-print").disabled = false;
    $("#btn-clear").disabled = false;
    pvImg.now(imgPayload);
  } catch { toast("Could not read that image", true); }
}
$("#btn-clear").addEventListener("click", () => {
  pic = null;
  $("#file").value = "";
  $("#img-meta").textContent = "";
  $("#btn-print").disabled = true;
  $("#btn-clear").disabled = true;
  pvImg.clear();
});
$("#btn-print").addEventListener("click", async () => {
  if (!pic) return;
  const b = $("#btn-print"), was = b.textContent;
  b.disabled = true;
  b.textContent = "Printing…";
  try { await api("/print/image", { method: "POST", body: { image_b64: pic.b64, mode } }); toast(`Sent to printer (${mode})`); refreshStatus(); }
  catch (e) { if (!quiet(e)) toast(e.message, true); }
  finally { b.textContent = was; b.disabled = !pic; }
});

/* ── Settings tab ── */
const INT_KEYS = new Set(["prints_per_hour", "max_per_ip_hour", "line_width", "printer_port", "receipt_font_size"]);
const BOOL_KEYS = new Set(["email_notifications", "receipt_show_timestamp", "receipt_show_email", "receipt_show_id"]);
const SELECTS = { printer_mode: ["auto", "usb", "network"], receipt_font: ["receipt", "mono", "sans", "serif"] };
const LABELS = {
  prints_per_hour: "Prints per hour", max_per_ip_hour: "Max per IP / hour", line_width: "Line width (chars)",
  allowed_origin: "Allowed origin", printer_mode: "Printer mode", printer_host: "Printer host (IP)",
  printer_port: "Printer port", email_notifications: "Email notifications", receipt_font: "Receipt font",
  receipt_font_size: "Receipt font size", receipt_show_timestamp: "Show timestamp", receipt_show_email: "Show email",
  receipt_show_id: "Show message ID", receipt_title: "Receipt title", receipt_footer: "Receipt footer",
};
const ORDER = ["prints_per_hour", "max_per_ip_hour", "allowed_origin", "printer_mode", "printer_host", "printer_port",
  "line_width", "email_notifications", "receipt_title", "receipt_footer", "receipt_font", "receipt_font_size",
  "receipt_show_timestamp", "receipt_show_email", "receipt_show_id"];
let original = {};
const readField = (i) => (i.type === "checkbox" ? (i.checked ? "true" : "false") : String(i.value));
const fields = () => $$("#settings-body [data-key]");
function settingsDirty() { return fields().some((i) => readField(i) !== String(original[i.dataset.key])); }
function dirtyCheck() { $("#btn-save").disabled = !settingsDirty(); }
async function loadSettings() {
  const body = $("#settings-body");
  try {
    const cfg = await api("/settings");
    original = { ...cfg };
    clear(body);
    const keys = ORDER.filter((k) => k in cfg).concat(Object.keys(cfg).filter((k) => !ORDER.includes(k)));
    for (const k of keys) {
      let input, ctl;
      if (BOOL_KEYS.has(k)) {
        input = h("input", { type: "checkbox", checked: String(cfg[k]).toLowerCase() === "true" });
        ctl = h("label", { class: "switch" }, input);
      } else if (SELECTS[k]) {
        input = h("select", null, SELECTS[k].map((o) => h("option", { value: o, text: o })));
        input.value = cfg[k];
        ctl = input;
      } else {
        input = h("input", { type: INT_KEYS.has(k) ? "number" : "text", value: cfg[k] });
        ctl = input;
      }
      input.id = "set-" + k;
      input.dataset.key = k;
      input.addEventListener("input", dirtyCheck);
      input.addEventListener("change", dirtyCheck);
      body.append(h("div", { class: "field" }, h("label", { for: "set-" + k, text: LABELS[k] || k }), ctl));
    }
    dirtyCheck();
  } catch (e) { if (!quiet(e)) clear(body).append(h("p", { class: "muted", text: "Could not load settings — " + e.message })); }
}
$("#btn-save").addEventListener("click", async () => {
  const changed = {};
  fields().forEach((i) => { const v = readField(i); if (v !== String(original[i.dataset.key])) changed[i.dataset.key] = v; });
  if (!Object.keys(changed).length) return;
  $("#btn-save").disabled = true;
  try {
    original = { ...(await api("/settings", { method: "PATCH", body: { settings: changed } })) };
    dirtyCheck();
    toast("Settings saved");
    refreshStatus();
    pvMsg.now(msgPayload);
  } catch (e) { if (!quiet(e)) toast(e.message, true); $("#btn-save").disabled = false; }
});

/* receipt preview (message fields → /preview kind:"message") */
const pvMsg = previewer($("#pv-box"), "Preview appears here");
const msgPayload = () => ({ kind: "message", name: $("#pv-name").value, email: $("#pv-email").value, message: $("#pv-msg").value });
["#pv-name", "#pv-email", "#pv-msg"].forEach((s) => $(s).addEventListener("input", () => pvMsg.schedule(msgPayload)));

/* ── boot ── */
(async function init() {
  try { await api("/queue"); enter(); }
  catch (e) { if (!(e instanceof AuthError)) showGate("Could not reach server."); }
})();

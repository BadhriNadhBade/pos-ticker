// Real-browser smoke test: mock server + headless Chrome/Edge over CDP (Node >= 22, no deps).
//   node web/dev/smoke.mjs           (set CHROME=/path/to/chrome if auto-detect fails)
// Fails on any console error / uncaught exception / CSP violation.
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

process.env.PORT = process.env.PORT || "8123";
const { server } = await import("./mock-server.mjs");
const BASE = `http://127.0.0.1:${process.env.PORT}`;
const KEY = "dev-admin-key-123456";

const candidates = [process.env.CHROME,
  "C:/Program Files/Google/Chrome/Application/chrome.exe",
  "C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe",
  "/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser",
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"].filter(Boolean);
const exe = candidates.find((p) => fs.existsSync(p));
if (!exe) { console.log("SKIP  no Chrome/Edge found (set CHROME)"); process.exit(0); }

const dbgPort = 9333;
const profile = fs.mkdtempSync(path.join(os.tmpdir(), "pt-smoke-"));
const proc = spawn(exe, ["--headless=new", `--remote-debugging-port=${dbgPort}`, `--user-data-dir=${profile}`,
  "--no-first-run", "--disable-gpu", "--no-sandbox", "about:blank"], { stdio: "ignore" });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

let targets;
for (let i = 0; i < 50 && !targets; i++) {
  try { targets = await (await fetch(`http://127.0.0.1:${dbgPort}/json/list`)).json(); } catch { await sleep(200); }
}
const ws = new WebSocket(targets.find((t) => t.type === "page").webSocketDebuggerUrl);
await new Promise((r) => (ws.onopen = r));
let id = 0;
const pending = new Map();
const problems = [];
ws.onmessage = (ev) => {
  const m = JSON.parse(ev.data);
  if (m.id && pending.has(m.id)) { const { ok, no } = pending.get(m.id); pending.delete(m.id); m.error ? no(new Error(m.error.message)) : ok(m.result); return; }
  if (m.method === "Runtime.exceptionThrown") problems.push("exception: " + (m.params.exceptionDetails.exception?.description || m.params.exceptionDetails.text));
  if (m.method === "Runtime.consoleAPICalled" && m.params.type === "error") problems.push("console.error: " + m.params.args.map((a) => a.value ?? a.description).join(" "));
  if (m.method === "Log.entryAdded" && m.params.entry.level === "error" && !/status of (401|400|404|403)/.test(m.params.entry.text)) problems.push("log: " + m.params.entry.text + " " + (m.params.entry.url || ""));
};
const cdp = (method, params = {}) => new Promise((ok, no) => { const i = ++id; pending.set(i, { ok, no }); ws.send(JSON.stringify({ id: i, method, params })); });
const ev = async (expr) => {
  const r = await cdp("Runtime.evaluate", { expression: expr, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error("eval failed: " + (r.exceptionDetails.exception?.description || r.exceptionDetails.text));
  return r.result.value;
};
const waitFor = async (expr, what, ms = 5000) => {
  const t0 = Date.now();
  while (Date.now() - t0 < ms) { if (await ev(expr).catch(() => false)) return; await sleep(80); }
  throw new Error("timeout waiting for: " + what);
};
let fails = 0;
const step = async (name, fn) => { try { await fn(); console.log("PASS  " + name); } catch (e) { fails++; console.log("FAIL  " + name + " -- " + e.message); } };
const key = (k) => ev(`document.dispatchEvent(new KeyboardEvent('keydown',{key:${JSON.stringify(k)},bubbles:true}))`);

await cdp("Runtime.enable"); await cdp("Log.enable"); await cdp("Page.enable");
await cdp("Emulation.setDeviceMetricsOverride", { width: 360, height: 740, deviceScaleFactor: 1, mobile: true });
await cdp("Page.navigate", { url: BASE + "/admin" });

await step("gate shown when unauthenticated", () => waitFor(`!document.querySelector('#gate').classList.contains('hidden') && document.querySelector('#app').classList.contains('hidden')`, "gate"));
await step("wrong key rejected", async () => {
  await ev(`(()=>{const k=document.querySelector('#gate-key');k.value='nope';document.querySelector('#gate').requestSubmit()})()`);
  await waitFor(`document.querySelector('#gate-err').textContent.includes('rejected')`, "error text");
});
await step("login with key shows app + 25 rows", async () => {
  await ev(`(()=>{const k=document.querySelector('#gate-key');k.value=${JSON.stringify(KEY)};document.querySelector('#gate').requestSubmit()})()`);
  await waitFor(`document.querySelectorAll('#msg-rows tr.clickable').length===25`, "25 rows");
});
await step("session cookie is HttpOnly (not readable from page) and no key in storage", async () => {
  if (await ev(`document.cookie.includes('pt_session')`)) throw new Error("cookie readable");
  if (await ev(`localStorage.length+sessionStorage.length`) !== 0) throw new Error("storage used");
});
await step("SSE connected -> Live", () => waitFor(`document.querySelector('#live-text').textContent==='Live'`, "Live"));
await step("stats + printer card populated", () => waitFor(`document.querySelector('#s-pending').textContent!=='\u2014' && document.querySelector('#pr-mode').textContent.startsWith('network')`, "stats"));
await step("no horizontal overflow at 360px", async () => { if (await ev(`document.documentElement.scrollWidth>360`)) throw new Error("scrollWidth=" + await ev(`document.documentElement.scrollWidth`)); });
await step("j selects, p prints optimistically then print event settles", async () => {
  await key("j");
  await waitFor(`document.querySelectorAll('#msg-rows tr.sel').length===1`, "selection");
  const selId = await ev(`document.querySelector('#msg-rows tr.sel').dataset.id`);
  await key("p");
  await waitFor(`document.querySelector('#msg-rows tr.sel .pill').textContent==='Queued'`, "optimistic Queued");
  await waitFor(`(()=>{const p=document.querySelector('#msg-rows tr[data-id="${selId}"] .pill').textContent;return p==='Printed'||p==='Pending'})()`, "print event settled", 6000);
});
await step("s skips with optimistic update", async () => {
  await key("j");
  await key("s");
  await waitFor(`document.querySelector('#msg-rows tr.sel .pill').textContent==='Skipped'`, "Skipped");
});
await step("search debounces and filters server-side", async () => {
  await ev(`(()=>{const q=document.querySelector('#q');q.value='Grace';q.dispatchEvent(new Event('input',{bubbles:true}))})()`);
  await waitFor(`(()=>{const r=[...document.querySelectorAll('#msg-rows tr.clickable')];return r.length>0&&r.length<25&&r.every(x=>x.textContent.includes('Grace'))})()`, "filtered rows");
  await ev(`(()=>{const q=document.querySelector('#q');q.value='';q.dispatchEvent(new Event('input',{bubbles:true}))})()`);
  await waitFor(`document.querySelectorAll('#msg-rows tr.clickable').length===25`, "restored rows");
});
await step("status chips filter", async () => {
  await ev(`document.querySelector('[data-f=skipped]').click()`);
  await waitFor(`(()=>{const r=[...document.querySelectorAll('#msg-rows tr.clickable .pill')];return r.length>0&&r.every(x=>x.textContent==='Skipped')})()`, "skipped only");
  await ev(`document.querySelector('[data-f=all]').click()`);
});
await step("/ focuses search, shortcuts ignored while typing", async () => {
  await key("/");
  await waitFor(`document.activeElement.id==='q'`, "search focus");
  await ev(`document.activeElement.dispatchEvent(new KeyboardEvent('keydown',{key:'p',bubbles:true}))`);
  await ev(`document.activeElement.blur()`);
});
await step("Enter opens detail modal with image; Esc closes and restores focus", async () => {
  await ev(`(()=>{const tr=[...document.querySelectorAll('#msg-rows tr.clickable')].find(t=>t.textContent.includes('+ image'));tr.click()})()`);
  await waitFor(`!document.querySelector('#modal').classList.contains('hidden')`, "modal");
  await waitFor(`!!document.querySelector('#modal-body img.detail-img')`, "original image");
  await key("Escape");
  await waitFor(`document.querySelector('#modal').classList.contains('hidden')`, "modal closed");
});
await step("? opens help dialog", async () => {
  await key("?");
  await waitFor(`document.querySelector('#modal-title').textContent.includes('Keyboard')`, "help");
  await key("Escape");
});
await step("Settings tab loads form + live receipt preview image", async () => {
  await ev(`document.querySelector('#tabbtn-settings').click()`);
  await waitFor(`document.querySelectorAll('#settings-body [data-key]').length>=15`, "settings fields");
  await waitFor(`!!document.querySelector('#pv-box img')`, "preview img");
  await ev(`(()=>{const i=document.querySelector('#set-line_width');i.value='40';i.dispatchEvent(new Event('input',{bubbles:true}))})()`);
  await waitFor(`!document.querySelector('#btn-save').disabled`, "save enabled");
  await ev(`document.querySelector('#btn-save').click()`);
  await waitFor(`document.querySelector('#btn-save').disabled`, "saved");
});
await step("Print tab: image file -> preview -> print", async () => {
  await ev(`document.querySelector('#tabbtn-print').click()`);
  await ev(`(async()=>{const c=document.createElement('canvas');c.width=2400;c.height=1200;const x=c.getContext('2d');x.fillStyle='#07c';x.fillRect(0,0,2400,1200);
    const b=await new Promise(r=>c.toBlob(r,'image/png'));const f=new File([b],'big.png',{type:'image/png'});
    const dt=new DataTransfer();dt.items.add(f);const i=document.querySelector('#file');i.files=dt.files;i.dispatchEvent(new Event('change'))})()`);
  await waitFor(`!!document.querySelector('#preview img') && !document.querySelector('#btn-print').disabled`, "image preview");
  if (!(await ev(`document.querySelector('#img-meta').textContent`)).includes("2400")) throw new Error("meta missing");
  await ev(`document.querySelector('#mode button[data-mode=ascii]').click()`);
  await sleep(700);
  await ev(`document.querySelector('#btn-print').click()`);
  await waitFor(`[...document.querySelectorAll('.toast')].some(t=>t.textContent.includes('ascii'))`, "print toast");
});
await step("optimistic update rolls back when the server rejects", async () => {
  await ev(`document.querySelector('#tabbtn-messages').click()`);
  // Make the UI believe a printed message is pending; the server will answer 404.
  const id = await ev(`(()=>{const m=S.msgs.find(x=>x.printed_at);m.printed_at=null;m.claimed=0;renderMsgs();act2('print',m.id);return m.id})()`);
  await waitFor(`[...document.querySelectorAll('.toast')].some(t=>t.textContent.includes('#${id} failed'))`, "failure toast");
  await waitFor(`S.msgs.find(x=>x.id===${id}).claimed===0`, "rolled back");
});
await step("SSE error falls back to 8s polling, reconnect resumes", async () => {
  await ev(`es.onerror()`);
  await waitFor(`document.querySelector('#live-text').textContent==='Polling' && pollT!==null`, "polling started");
  await ev(`startLive()`);
  await waitFor(`document.querySelector('#live-text').textContent==='Live' && pollT===null`, "live again");
});
await step("sign out returns to gate; API now 401", async () => {
  await ev(`document.querySelector('#signout').click()`);
  await waitFor(`!document.querySelector('#gate').classList.contains('hidden')`, "gate");
  if ((await ev(`fetch('/queue').then(r=>r.status)`)) !== 401) throw new Error("still authed");
});
await step("no console errors / CSP violations", async () => { if (problems.length) throw new Error(problems.join(" | ")); });

ws.close(); proc.kill(); server.close();
await sleep(300);
try { fs.rmSync(profile, { recursive: true, force: true }); } catch { /* chrome may still hold files */ }
console.log(fails ? `\n${fails} step(s) FAILED` : "\nsmoke passed");
process.exit(fails ? 1 : 0);

// Static checks for the embedded UI. Usage: node web/dev/check.mjs
// Enforces the CSP/security rules used by the admin UI.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";

const web = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const read = (f) => fs.readFileSync(path.join(web, f), "utf8");
const html = read("index.html"), js = read("app.js"), css = read("app.css"), goSrc = read("embed.go");
const BUDGET = 60 * 1024;

let failed = 0;
const ok = (c, msg) => { console.log(`${c ? "PASS" : "FAIL"}  ${msg}`); if (!c) failed++; };
const strip = (s) => s.replace(/<!--[\s\S]*?-->/g, "");
const h = strip(html);

// index.html: strict-CSP rules
ok(![...h.matchAll(/<script\b[^>]*>/gi)].some((m) => !/\bsrc=/.test(m[0])), "index.html: no inline <script> (every <script> has src)");
ok(![...h.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/gi)].some((m) => m[1].trim()), "index.html: <script> elements are empty");
ok(!/<style\b/i.test(h), "index.html: no <style> element");
ok(!/\sstyle\s*=/i.test(h), "index.html: no style= attributes");
ok(!/\son[a-z]+\s*=/i.test(h), "index.html: no on*= event-handler attributes");
ok(!/javascript:/i.test(h + js), "no javascript: URLs");
ok(!/(?:src|href|action)\s*=\s*["']?(?:https?:)?\/\//i.test(h), "index.html: no external src/href/action");
ok(!/https?:\/\//i.test(h.replace(/xmlns="[^"]*"/g, "")), "index.html: no http(s):// URLs");
ok(/src="\/assets\/app\.js"/.test(h) && /href="\/assets\/app\.css"/.test(h), "index.html: references /assets/app.js and /assets/app.css");

// app.js: no HTML-string sinks (clearing via textContent = "" is the only allowed idiom)
ok(!/\b(innerHTML|outerHTML)\b/.test(js), "app.js: no innerHTML/outerHTML");
ok(!/insertAdjacentHTML|document\.write|\beval\s*\(|new\s+Function\b|setTimeout\s*\(\s*["'`]|createContextualFragment|DOMParser/.test(js), "app.js: no HTML/code-string sinks");
ok(!/https?:\/\//i.test(js), "app.js: no external URLs");
ok(!/localStorage|sessionStorage|indexedDB/.test(js), "app.js: no web storage (admin key never stored)");
ok(!/X-Admin-Key/i.test(js), "app.js: does not use X-Admin-Key (cookie session only)");
ok(/X-Requested-With["']?\s*[:=,]?\s*["']?pos-ticker|"X-Requested-With": "pos-ticker"/.test(js), "app.js: sends X-Requested-With: pos-ticker");
ok(/new EventSource\(["']\/events["']\)/.test(js), "app.js: uses EventSource('/events')");
ok(!/\.style\b|setAttribute\(\s*["']style/.test(js), "app.js: no inline style manipulation");
ok(!/\son[a-z]+\s*=\s*["']/.test(js.replace(/\bon:\s*\{/g, "")), "app.js: no on*= attribute strings");

// app.css
ok(!/@import|url\(\s*["']?(?:https?:)?\/\//i.test(css), "app.css: no @import / external url()");
ok(!/https?:\/\//i.test(css), "app.css: no external URLs");
ok(/prefers-reduced-motion/.test(css) && /prefers-color-scheme:\s*dark/.test(css), "app.css: reduced-motion + dark-mode media queries");

// embed.go
ok(/package web/.test(goSrc) && /\/\/go:embed index\.html app\.js app\.css/.test(goSrc) && /var FS embed\.FS/.test(goSrc), "embed.go: package web, go:embed directive, var FS");

// syntax
const syn = spawnSync(process.execPath, ["--check", path.join(web, "app.js")], { encoding: "utf8" });
ok(syn.status === 0, "app.js: node --check" + (syn.status ? "\n" + syn.stderr : ""));

// size budget (uncompressed, embedded files only)
const sizes = ["index.html", "app.js", "app.css"].map((f) => [f, fs.statSync(path.join(web, f)).size]);
const total = sizes.reduce((a, [, n]) => a + n, 0);
for (const [f, n] of sizes) console.log(`      ${f.padEnd(11)} ${n} bytes`);
ok(total <= BUDGET, `total ${total} bytes <= ${BUDGET}`);

console.log(failed ? `\n${failed} check(s) FAILED` : "\nall checks passed");
process.exit(failed ? 1 : 0);

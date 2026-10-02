"use strict";

const T = {
  en: {
    location: "Location", ipv4: "Test IPv4", ipv6: "Test IPv6", yourip: "Your IP",
    speedTitle: "Speed test from your browser",
    speedHint: "Measures latency, download and upload between your browser and this server. No software needed.",
    latency: "Latency", download: "Download", upload: "Upload", start: "Start test",
    netTitle: "Network tests from the server", run: "Run",
    netHint: "Defaults to your own IP — see the route from the server back to you.",
    footer: "Public tests only. Private and reserved addresses are refused.",
    measuring: "Measuring…", done: "Done", jitter: "jitter",
    worldTitle: "Latency from around the world",
    worldHint: "Ping to this server from probes in other cities, measured periodically.",
    wPlace: "Location", wNetwork: "Network", wLatency: "Latency", wLoss: "Loss",
    wPending: "The first measurement is running. Check back in a few minutes.",
    wMeasured: "Measured {ago}, refreshed every {every}", wData: "data",
    ago: { now: "just now", min: "{n} min ago", h: "{n} h ago" }, hours: "{n} h", minutes: "{n} min",
    err: {
      bad_target: "Enter a valid IP address or hostname.",
      not_public: "Only public addresses can be tested.",
      no_address: "That hostname has no address of the selected IP version.",
      rate_limited: "Too many tests. Wait a minute and try again.",
      busy: "The server is busy with other tests. Try again shortly.",
      failed: "The test could not run.",
      network: "Connection error.",
    },
  },
  ro: {
    location: "Locație", ipv4: "IPv4 de test", ipv6: "IPv6 de test", yourip: "IP-ul tău",
    speedTitle: "Test de viteză din browser",
    speedHint: "Măsoară latența, download-ul și upload-ul între browserul tău și acest server. Nu trebuie instalat nimic.",
    latency: "Latență", download: "Download", upload: "Upload", start: "Pornește testul",
    netTitle: "Teste de rețea de pe server", run: "Rulează",
    netHint: "Implicit e IP-ul tău — vezi ruta de la server înapoi către tine.",
    footer: "Doar teste publice. Adresele private și rezervate sunt refuzate.",
    measuring: "Se măsoară…", done: "Gata", jitter: "jitter",
    worldTitle: "Latența din alte orașe",
    worldHint: "Ping către acest server din sonde aflate în alte orașe, măsurat periodic.",
    wPlace: "Locație", wNetwork: "Rețea", wLatency: "Latență", wLoss: "Pierderi",
    wPending: "Prima măsurătoare e în curs. Revino în câteva minute.",
    wMeasured: "Măsurat {ago}, actualizat la fiecare {every}", wData: "date",
    ago: { now: "chiar acum", min: "acum {n} min", h: "acum {n} h" }, hours: "{n} h", minutes: "{n} min",
    err: {
      bad_target: "Introdu o adresă IP sau un hostname valid.",
      not_public: "Se pot testa doar adrese publice.",
      no_address: "Hostname-ul nu are adresă pentru versiunea IP aleasă.",
      rate_limited: "Prea multe teste. Așteaptă un minut și încearcă din nou.",
      busy: "Serverul rulează alte teste. Încearcă din nou în scurt timp.",
      failed: "Testul nu a putut rula.",
      network: "Eroare de conexiune.",
    },
  },
};

const $ = (id) => document.getElementById(id);
let lang = "en";
let info = null;
let world = null;

function pickLang() {
  try {
    const saved = localStorage.getItem("lg-lang");
    if (saved && T[saved]) return saved;
  } catch (_) { /* storage unavailable */ }
  return (navigator.language || "").toLowerCase().startsWith("ro") ? "ro" : "en";
}

function setLang(l) {
  lang = l;
  document.documentElement.lang = l;
  document.querySelectorAll("[data-t]").forEach((el) => {
    const v = T[l][el.dataset.t];
    if (typeof v === "string") el.textContent = v;
  });
  document.querySelectorAll("[data-lang]").forEach((b) => b.classList.toggle("on", b.dataset.lang === l));
  if (world) renderWorld();
  try { localStorage.setItem("lg-lang", l); } catch (_) { /* ignore */ }
}

const errText = (code) => T[lang].err[code] || T[lang].err.failed;

async function loadInfo() {
  const r = await fetch("api/info", { cache: "no-store" });
  info = await r.json();
  document.title = info.brand + " · Looking Glass";
  const brand = $("brand");
  brand.textContent = info.brand;
  if (info.brandURL) brand.href = info.brandURL;
  $("location").textContent = [info.location, info.datacenter].filter(Boolean).join(" · ") || "—";
  $("ipv4").textContent = info.ipv4 || "—";
  $("ipv6").textContent = info.ipv6 || "—";
  if (!info.ipv6) $("ipv6card").hidden = true;
  $("yourip").textContent = info.client.ip;
  $("target").value = info.client.ip;
  $("family").value = String(info.client.family);
}

function copyable(el) {
  el.addEventListener("click", async () => {
    const v = el.textContent.trim();
    if (!v || v === "—") return;
    try {
      await navigator.clipboard.writeText(v);
      el.classList.add("copied");
      setTimeout(() => el.classList.remove("copied"), 1200);
    } catch (_) { /* clipboard blocked */ }
  });
}

// ---- latency from other cities (measured by the server, read from its cache) ----

const fill = (tpl, vars) => tpl.replace(/\{(\w+)\}/g, (_, k) => vars[k]);

function ago(iso) {
  const min = Math.round((Date.now() - Date.parse(iso)) / 60000);
  const a = T[lang].ago;
  if (!(min >= 1)) return a.now;
  return min < 60 ? fill(a.min, { n: min }) : fill(a.h, { n: Math.round(min / 60) });
}

function every(minutes) {
  return minutes % 60 === 0 ? fill(T[lang].hours, { n: minutes / 60 }) : fill(T[lang].minutes, { n: minutes });
}

function cell(row, text, cls) {
  const td = document.createElement("td");
  if (cls) td.className = cls;
  if (text !== undefined) td.textContent = text;
  row.appendChild(td);
  return td;
}

function renderWorld() {
  const tbody = $("world-rows");
  const meta = $("world-meta");
  tbody.textContent = "";
  meta.textContent = "";
  const rows = [...(world.results || [])].sort((a, b) => a.avg - b.avg);
  if (!rows.length) {
    meta.textContent = T[lang].wPending;
    return;
  }
  const top = Math.max(...rows.map((r) => r.avg), 1);
  for (const r of rows) {
    const tr = document.createElement("tr");
    const place = cell(tr, r.label);
    const cc = document.createElement("span");
    cc.className = "cc";
    cc.textContent = r.country;
    place.appendChild(cc);
    cell(tr, r.network, "net");
    const lat = cell(tr, undefined, "num");
    const wrap = document.createElement("span");
    wrap.className = "lat";
    const bar = document.createElement("span");
    bar.className = "bar";
    bar.style.width = Math.max(2, Math.round((r.avg / top) * 80)) + "px";
    wrap.append(bar, document.createTextNode(r.avg.toFixed(1) + " ms"));
    lat.appendChild(wrap);
    cell(tr, Math.round(r.loss) + "%", "num" + (r.loss > 0 ? " warn" : ""));
    tbody.appendChild(tr);
  }
  meta.append(fill(T[lang].wMeasured, { ago: ago(world.measuredAt), every: every(world.intervalMinutes) }) + " · " + T[lang].wData + ": ");
  const a = document.createElement("a");
  a.href = "https://globalping.io";
  a.rel = "noopener";
  a.textContent = "Globalping";
  meta.appendChild(a);
}

async function loadWorld() {
  const r = await fetch("api/world", { cache: "no-store" });
  const data = await r.json();
  if (!data.enabled) return;
  world = data;
  $("world").hidden = false;
  renderWorld();
}

// ---- network tests (server-sent events read through fetch so that HTTP
// errors keep their JSON body) ----

let running = null;

async function runTest(ev) {
  ev.preventDefault();
  if (running) running.abort();
  const out = $("out");
  out.hidden = false;
  out.textContent = "";
  const go = $("go");
  go.disabled = true;

  const tool = $("tool").value;
  const family = $("family").value;
  const target = $("target").value.trim();
  const qs = new URLSearchParams({ tool, family, target });
  const ctrl = new AbortController();
  running = ctrl;

  const write = (text, cls) => {
    const span = document.createElement("span");
    if (cls) span.className = cls;
    span.textContent = text + "\n";
    out.appendChild(span);
  };

  try {
    const r = await fetch("api/run?" + qs, { signal: ctrl.signal, cache: "no-store" });
    if (!r.ok) {
      let code = "failed";
      try { code = (await r.json()).error || code; } catch (_) { /* not json */ }
      write(errText(code), "err");
      return;
    }
    const reader = r.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf("\n\n")) >= 0) {
        const block = buf.slice(0, i);
        buf = buf.slice(i + 2);
        let event = "message", data = "";
        for (const l of block.split("\n")) {
          if (l.startsWith("event: ")) event = l.slice(7);
          else if (l.startsWith("data: ")) data = JSON.parse(l.slice(6));
        }
        if (event === "target") write(`$ ${tool} ${data}`);
        else if (event === "line") write(data);
        else if (event === "error") write(errText(data), "err");
      }
    }
  } catch (e) {
    if (e.name !== "AbortError") write(errText("network"), "err");
  } finally {
    if (running === ctrl) running = null;
    go.disabled = false;
  }
}

// ---- browser speed test ----

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const mbps = (bytes, ms) => (bytes * 8) / (ms * 1000);
const fmt = (v) => (v >= 100 ? v.toFixed(0) : v.toFixed(1));

async function measureLatency() {
  const samples = [];
  for (let i = 0; i < 21; i++) {
    const t0 = performance.now();
    await fetch("speed/ping?" + i + "-" + Math.random(), { cache: "no-store" });
    if (i > 0) samples.push(performance.now() - t0); // first one pays for connection setup
  }
  const sorted = [...samples].sort((a, b) => a - b);
  const median = sorted[Math.floor(sorted.length / 2)];
  let jitter = 0;
  for (let i = 1; i < samples.length; i++) jitter += Math.abs(samples[i] - samples[i - 1]);
  return { median, jitter: jitter / (samples.length - 1) };
}

// Transfers run on several parallel requests, like common speed tests: a
// single stream rarely fills a fast line at tens of milliseconds of latency.
// The total size is split across the streams, so a test costs the same
// traffic budget whatever the stream count.
function streamCount() {
  return Math.max(1, Math.min(8, (info && info.speedStreams) || 1));
}

function split(totalMB) {
  const n = Math.min(streamCount(), totalMB);
  return { n, each: Math.max(1, Math.floor(totalMB / n)) };
}

function httpError(r) {
  return new Error(r.status === 429 ? "rate_limited" : "busy");
}

// Runs one transfer per stream and reports the combined rate. Bytes from a
// stream that fails midway still count; it fails only if every stream does.
async function parallel(totalMB, one, onProgress) {
  const { n, each } = split(totalMB);
  const t0 = performance.now();
  let bytes = 0;
  const add = (k) => {
    bytes += k;
    if (onProgress) onProgress(mbps(bytes, performance.now() - t0));
  };
  const results = await Promise.allSettled(Array.from({ length: n }, () => one(each, add)));
  const ms = performance.now() - t0;
  if (results.every((r) => r.status === "rejected")) throw results[0].reason;
  return { bytes, ms };
}

async function downloadOne(mb, add) {
  const r = await fetch("speed/down?mb=" + mb, { cache: "no-store" });
  if (!r.ok) throw httpError(r);
  const reader = r.body.getReader();
  for (;;) {
    const { value, done } = await reader.read();
    if (done) return;
    add(value.length);
  }
}

let randomChunk = null;
function uploadBlob(mb) {
  if (!randomChunk) {
    randomChunk = new Uint8Array(1 << 20);
    for (let i = 0; i < randomChunk.length; i += 65536) {
      crypto.getRandomValues(randomChunk.subarray(i, i + 65536));
    }
  }
  return new Blob(Array(mb).fill(randomChunk), { type: "application/octet-stream" });
}

// fetch cannot report upload progress, so a stream counts once it is done.
async function uploadOne(mb, add) {
  const body = uploadBlob(mb);
  const r = await fetch("speed/up", { method: "POST", body, cache: "no-store" });
  if (!r.ok) throw httpError(r);
  await r.text();
  add(body.size);
}

// Run a short probe, then size the real transfer to last about six seconds.
function nextSize(probe, max) {
  const rate = probe.bytes / probe.ms; // bytes per ms
  return Math.max(1, Math.min(max, Math.round((rate * 6000) / (1 << 20))));
}

async function speedTest() {
  const btn = $("speed");
  const status = $("speed-status");
  btn.disabled = true;
  status.className = "status";
  status.textContent = T[lang].measuring;
  ["m-lat", "m-down", "m-up"].forEach((id) => ($(id).textContent = "—"));
  $("m-jit").textContent = "";
  const max = info ? info.speedMaxMB : 100;

  try {
    const lat = await measureLatency();
    $("m-lat").textContent = lat.median.toFixed(1);
    $("m-jit").textContent = `${T[lang].jitter} ${lat.jitter.toFixed(1)} ms`;

    const show = (v) => ($("m-down").textContent = fmt(v));
    const dProbe = await parallel(Math.min(16, max), downloadOne, show);
    const d = await parallel(nextSize(dProbe, max), downloadOne, show);
    show(mbps(d.bytes, d.ms));

    await sleep(200);
    const uProbe = await parallel(Math.min(8, max), uploadOne);
    $("m-up").textContent = fmt(mbps(uProbe.bytes, uProbe.ms));
    const u = await parallel(nextSize(uProbe, max), uploadOne);
    $("m-up").textContent = fmt(mbps(u.bytes, u.ms));

    status.textContent = T[lang].done;
  } catch (e) {
    status.className = "status err";
    status.textContent = errText(e.message in T[lang].err ? e.message : "network");
  } finally {
    btn.disabled = false;
  }
}

document.addEventListener("DOMContentLoaded", () => {
  setLang(pickLang());
  document.querySelectorAll("[data-lang]").forEach((b) => b.addEventListener("click", () => setLang(b.dataset.lang)));
  copyable($("ipv4"));
  copyable($("ipv6"));
  $("run").addEventListener("submit", runTest);
  $("speed").addEventListener("click", speedTest);
  loadInfo().catch(() => {
    $("speed-status").textContent = errText("network");
  });
  loadWorld().catch(() => {});
});

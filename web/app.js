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
});

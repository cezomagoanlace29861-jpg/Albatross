// Subscriptions for devices without the app (iPhone, iPad, Mac): the
// account's servers as any client app reads them, a base64 list of links,
// by country, every server named after the service, refreshed by the app
// itself. The address carries a token of its own (a year, void when the
// password changes), so the apps need no sign-in.
import { DAY, accountOf, edgeLinks, sign, tag, verify } from "./lib.js";
import { LIST_KEYS } from "./files.js";
import { COUNTRIES, flag, hint } from "./countries.js";
import { cleanLink } from "./links.js";

export const SUB_TTL = 365 * DAY;

/** The token in a subscription address. */
export const subToken = (env, user, now) => sign({ u: user.id, v: user.token_version, sub: 1, exp: now + SUB_TTL }, env.TOKEN_SECRET);

/** The payload of a subscription token, or null. */
export async function readSubToken(env, token, now) {
  const p = await verify(token, env.TOKEN_SECRET, now);
  return p && p.sub === 1 && p.u ? p : null;
}

// In slices: spread into one call, a list over about 125 KB (a full hot
// list of long links) exceeds the engine's argument limit and throws.
const b64 = (s) => {
  const bytes = new TextEncoder().encode(s);
  let bin = "";
  for (let i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(bin);
};
const unb64 = (s) => new TextDecoder().decode(Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0)));

/** The name a link carries (its fragment, or a vmess link's "ps"). */
export function nameOf(link) {
  if (link.startsWith("vmess://")) {
    try {
      return String(JSON.parse(unb64(link.slice(8))).ps || "");
    } catch {
      return "";
    }
  }
  const i = link.indexOf("#");
  if (i < 0) return "";
  try {
    return decodeURIComponent(link.slice(i + 1));
  } catch {
    return link.slice(i + 1);
  }
}

/** The link with its name replaced. */
export function rename(link, name) {
  if (link.startsWith("vmess://")) {
    try {
      const j = JSON.parse(unb64(link.slice(8)));
      j.ps = name;
      return "vmess://" + b64(JSON.stringify(j));
    } catch {
      return link;
    }
  }
  const i = link.indexOf("#");
  return (i < 0 ? link : link.slice(0, i)) + "#" + encodeURIComponent(name);
}

/** The links of a list file, each with the country its name says. */
export function parseList(text) {
  const out = [];
  for (const l of String(text || "").split(/\r?\n/)) {
    const link = l.trim();
    if (!link || link.startsWith("#") || !/^[a-z0-9+.-]+:\/\//i.test(link)) continue;
    out.push({ link, country: hint(nameOf(link)) });
  }
  return out;
}

// The schemes the connection apps on Apple devices (V2Box, Streisand,
// Shadowrocket, Hiddify, sing-box) take. The app's own protocols and
// plain proxies are left out of a subscription.
const APPLE_SCHEMES = new Set(["vmess", "vless", "trojan", "ss", "hysteria2", "hy2", "tuic", "wireguard", "wg"]);

/** Whether a link is one the Apple apps can use. */
export const forApple = (e) => APPLE_SCHEMES.has(e.link.slice(0, e.link.indexOf("://")).toLowerCase());

// How many servers a subscription carries at least, and how far into the
// fresh list the top-up looks.
export const SUB_MIN = 40;
const TOPUP_FROM = 400;

/**
 * The pool a subscription is built from: the hot list (tested through for
 * real, in order) and, when it is short, a top-up from the fresh list
 * (newly seen, their address answering), a different sample each time so
 * that refreshing the subscription brings other servers to try. Servers
 * inside the country and ones the Apple apps cannot use are left out;
 * keep writes each of the rest anew, or leaves it out with null.
 */
export function pool(hot, fresh, min = SUB_MIN, pick = Math.random, keep = (e) => e, accept = forApple) {
  const seen = new Set();
  const out = [];
  for (const e of parseList(hot)) {
    if (e.country !== "IR" && accept(e) && !seen.has(e.link)) {
      seen.add(e.link);
      const k = keep(e);
      if (k) out.push(k);
    }
  }
  if (out.length >= min) return tcpFirst(out);
  const cands = [];
  for (const e of parseList(fresh)) {
    if (cands.length >= TOPUP_FROM) break;
    if (e.country !== "IR" && accept(e) && !seen.has(e.link)) {
      seen.add(e.link);
      const k = keep(e);
      if (k) cands.push(k);
    }
  }
  // A random sample of the candidates, in their order.
  const want = Math.min(min - out.length, cands.length);
  const chosen = new Set();
  while (chosen.size < want) chosen.add(Math.floor(pick() * cands.length) % cands.length);
  for (const [i, e] of cands.entries()) if (chosen.has(i)) out.push(e);
  return tcpFirst(out);
}

// UDP is throttled on many networks here: servers over it go after the rest.
const UDP_SCHEMES = new Set(["hysteria2", "hy2", "tuic"]);
const overUDP = (e) => UDP_SCHEMES.has(e.link.slice(0, e.link.indexOf("://")).toLowerCase());
export const tcpFirst = (entries) => [...entries.filter((e) => !overUDP(e)), ...entries.filter(overUDP)];

/** The countries in a list, most servers first. */
export function countriesOf(entries) {
  const n = new Map();
  for (const e of entries) if (e.country && e.country !== "IR") n.set(e.country, (n.get(e.country) || 0) + 1);
  return [...n].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([code, count]) => ({ code, name: COUNTRIES[code] || code, flag: flag(code), count }));
}

/**
 * The subscription for a choice: "" every server, a country code that
 * country's, "edge" the project's own edge. Each server is named after the
 * service; the servers' own names and sources never show.
 */
export function build(entries, choice, brand, edge, updated = "") {
  let list = entries;
  let label = "All servers";
  if (choice === "edge") {
    list = [];
  } else if (choice) {
    list = entries.filter((e) => e.country === choice);
    label = COUNTRIES[choice] || choice;
  } else {
    list = entries.filter((e) => e.country !== "IR");
  }
  const servers = [];
  if (!choice || choice === "edge") for (const [i, l] of edge.entries()) servers.push({ link: l, name: `${brand} Edge ${i + 1}` });
  for (const [i, e] of list.entries()) {
    const where = e.country ? `${flag(e.country)} ${COUNTRIES[e.country] || e.country}` : "Server";
    servers.push({ link: e.link, name: `${brand} ${where} ${i + 1} · ${protoOf(e.link)}` });
  }
  const links = servers.map((s) => rename(s.link, s.name));
  // The title carries the list's time, so a refresh visibly brings a newer list.
  const when = stamp(updated);
  return { title: `${brand} ${label}${when ? " · " + when : ""}`, body: b64(links.join("\n") + "\n"), count: links.length, servers };
}

/** The protocol a link's scheme names, as the apps show it. */
export function protoOf(link) {
  const s = link.slice(0, link.indexOf("://")).toLowerCase();
  return { ss: "shadowsocks", hy2: "hysteria2", wg: "wireguard" }[s] || s;
}

/** "26 Oct 11:05 UTC" for an ISO time, or "" when there is none. */
export function stamp(iso) {
  const t = iso ? new Date(iso) : null;
  if (!t || Number.isNaN(t.getTime())) return "";
  const months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
  const two = (n) => String(n).padStart(2, "0");
  return `${t.getUTCDate()} ${months[t.getUTCMonth()]} ${two(t.getUTCHours())}:${two(t.getUTCMinutes())} UTC`;
}

/** The headers client apps read: the allowance and the name. */
export function headers(title, account, interval = 1) {
  const h = new Headers({
    "Content-Type": "text/plain; charset=utf-8",
    "Cache-Control": "no-store",
    "Content-Disposition": `attachment; filename="${title.replace(/[^\w .-]+/g, "")}.txt"`,
    "Profile-Title": "base64:" + b64(title),
    "Profile-Update-Interval": String(interval),
    "X-Content-Type-Options": "nosniff",
  });
  const info = [`upload=0`, `download=${account.used_bytes || 0}`];
  if (account.quota_bytes > 0) info.push(`total=${account.quota_bytes}`);
  if (account.expires_at > 0) info.push(`expire=${account.expires_at}`);
  h.set("Subscription-Userinfo", info.join("; "));
  return h;
}

// The collector seals its lists with a key per collection (the Go side:
// internal/sources SealKeyed): "ALBK1", the key's id (8 bytes, big
// endian), then AES-256-GCM with the nonce first.
const KEYED_MAGIC = "ALBK1";

/** The id of the key a sealed list needs, or null when it is not one. */
export function keyedID(bytes) {
  if (bytes.length < KEYED_MAGIC.length + 8) return null;
  for (let i = 0; i < KEYED_MAGIC.length; i++) if (bytes[i] !== KEYED_MAGIC.charCodeAt(i)) return null;
  let id = 0n;
  for (let i = 0; i < 8; i++) id = (id << 8n) | BigInt(bytes[KEYED_MAGIC.length + i]);
  return Number(id);
}

/** Opens a sealed list with the key of its id among keys ({id, key: hex}); "" when it cannot be opened. */
export async function openKeyed(keys, bytes) {
  const id = keyedID(bytes);
  if (id === null) return "";
  const k = keys.find((x) => x.id === id);
  if (!k || !/^[0-9a-f]{64}$/.test(k.key)) return "";
  const raw = Uint8Array.from(k.key.match(/../g), (h) => parseInt(h, 16));
  const body = bytes.subarray(KEYED_MAGIC.length + 8);
  if (body.length < 12 + 16) return "";
  try {
    const key = await crypto.subtle.importKey("raw", raw, "AES-GCM", false, ["decrypt"]);
    const plain = await crypto.subtle.decrypt({ name: "AES-GCM", iv: body.subarray(0, 12) }, key, body.subarray(12));
    return new TextDecoder().decode(plain);
  } catch {
    return "";
  }
}

/** The lists' keys the collector stored (LIST_KEYS), or []. */
export async function storedListKeys(env) {
  if (!env.FILES) return [];
  try {
    const o = await env.FILES.get(LIST_KEYS);
    const all = o ? await o.json() : [];
    return Array.isArray(all) ? all : [];
  } catch {
    return [];
  }
}

/**
 * A server list as the service has it: the sealed copy in its own
 * storage, opened with the collector's keys (the plain copy while one
 * is still published), or GitHub where there is no storage.
 */
export async function listFile(env, name) {
  if (env.FILES) {
    const sealed = await env.FILES.get("list/servers/" + name + ".sealed");
    if (sealed) {
      const text = await openKeyed(await storedListKeys(env), new Uint8Array(await sealed.arrayBuffer()));
      if (text) return text;
    }
    const o = await env.FILES.get("list/servers/" + name);
    return o ? o.text() : "";
  }
  const up = await fetch("https://raw.githubusercontent.com/Albatrossvpn/Albatross/servers/" + name, { cf: { cacheTtl: 600, cacheEverything: true } });
  return up.ok ? up.text() : "";
}

export const hotList = (env) => listFile(env, "hot.txt");

/** When the lists were last published (meta.json), as an ISO time, or "". */
export async function listUpdated(env) {
  try {
    const m = JSON.parse((await listFile(env, "meta.json")) || "{}");
    return typeof m.updated === "string" ? m.updated : "";
  } catch {
    return "";
  }
}

/**
 * A server as the apps on Apple devices take it: its link in the standard
 * form the engine's reading of it makes (links.js), still named as it
 * came, or null when the engine would refuse it.
 */
export function cleanEntry(e) {
  const link = cleanLink(e.link, nameOf(e.link));
  return link ? { ...e, link } : null;
}

/**
 * The servers a subscription offers right now (pool), each in the standard
 * form. pick draws the top-up from the fresh list: a seeded one (dayPick)
 * keeps the pool, and so each account's slice, the same all day.
 */
export async function subPool(env, pick = Math.random) {
  // Only servers the collector connected through and measured (hot.txt):
  // no top-up from lists nobody has tested.
  return pool(await listFile(env, "hot.txt"), "", SUB_MIN, pick, cleanEntry);
}

/**
 * The servers the apps are offered a slice of: the same pool as a
 * subscription's, in every protocol the app reads, the links as they came.
 */
export async function appPool(env, pick = Math.random) {
  // Only servers the collector connected through and measured (hot.txt).
  return pool(await listFile(env, "hot.txt"), "", SUB_MIN, pick, (e) => e, () => true);
}

// ---------- Each account's slice ----------
//
// No account gets the whole pool: each gets a slice of it, the same all
// day and another the next, different from other accounts' (sliceSeed). A
// paying user with a script, or a copy of the app with its checks cut out,
// learns a few dozen servers a day, not the collection. The slice keeps
// what made the pool worth having: every country in it, the best-tested
// servers of each first, and a spread of the ways through the filter.

export const SLICE_BASE = 60; // the size of a slice with few countries
export const SLICE_MAX = 120; // ... and with many
export const SLICE_PER_COUNTRY = 3; // what each country gets at least, when it has them
// A country's own subscription: fewer, so that asking for one country after
// another does not add up to the pool.
export const SLICE_COUNTRY = 20;
const SLICE_WINDOW = 3; // a country's slice is drawn from its best SLICE_WINDOW × as many

/** A number for a string (FNV-1a, 32 bits). */
function fnv(s) {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h >>> 0;
}

/** A pick (as Math.random) that gives the same numbers for the same seed. */
export function seededPick(seed) {
  let a = fnv(String(seed)) || 1;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/** The same pick for everyone all day (UTC): the pool's top-up does not change between requests. */
export const dayPick = (t) => seededPick("pool|" + Math.floor(t / DAY));

/**
 * How a server gets through: Reality, a CDN-frontable transport (WebSocket,
 * HTTPUpgrade, XHTTP, gRPC), QUIC (Hysteria2, TUIC), or its scheme.
 */
export function carrierOf(link) {
  const scheme = link.slice(0, link.indexOf("://")).toLowerCase();
  if (UDP_SCHEMES.has(scheme)) return "quic";
  let q = "";
  if (scheme === "vmess") {
    try {
      const j = JSON.parse(unb64(link.slice(8).split("#")[0]));
      q = `type=${j.net || ""}&security=${j.tls || ""}`;
    } catch {}
  } else {
    const i = link.indexOf("?");
    q = i < 0 ? "" : link.slice(i + 1).split("#")[0];
  }
  q = q.toLowerCase();
  if (/(^|&)security=reality(&|$)/.test(q)) return "reality";
  if (/(^|&)(type|net)=(ws|httpupgrade|xhttp|splithttp)(&|$)/.test(q)) return "cdn";
  if (/(^|&)(type|net)=grpc(&|$)/.test(q)) return "grpc";
  return scheme;
}

/** The seed of an account's slice for the day (UTC) of t: not to be guessed without the service's secret. */
export const sliceSeed = (env, userId, t) => tag(env.TOKEN_SECRET, `servers|${userId}|${Math.floor(t / DAY)}`);

/**
 * An account's slice of a pool (entries in the pool's order, best first):
 * every country in it, each with SLICE_PER_COUNTRY servers at least (as
 * many as it has, when fewer) and more for the countries with many, in all
 * SLICE_BASE, up to SLICE_MAX when there are many countries. A country's
 * servers come from its best few, drawn by the seed and spread over the
 * ways through (carrierOf). In the pool's order. cap bounds the total
 * (SLICE_COUNTRY for one country's), though never below one server for
 * each country.
 */
export function slice(entries, seed, cap = SLICE_MAX, base = SLICE_BASE) {
  const groups = new Map();
  for (const [i, e] of entries.entries()) {
    if (e.country === "IR") continue;
    const c = e.country || "";
    if (!groups.has(c)) groups.set(c, []);
    groups.get(c).push({ e, i });
  }
  const size = [...groups.values()].reduce((n, g) => n + g.length, 0);
  const countries = [...groups.keys()].sort();
  let total = Math.min(size, Math.max(base, Math.min(Math.max(cap, base), countries.length * SLICE_PER_COUNTRY)));
  // More countries than the slice holds: one each, so every one is still there.
  total = Math.max(total, Math.min(size, countries.length));
  total = Math.min(total, Math.max(cap, Math.min(size, countries.length)));
  const per = Math.max(1, Math.min(SLICE_PER_COUNTRY, Math.floor(total / Math.max(1, countries.length))));
  const quota = new Map(countries.map((c) => [c, Math.min(per, groups.get(c).length)]));
  let left = total - [...quota.values()].reduce((a, b) => a + b, 0);
  // The rest by size (the largest remainder share first, D'Hondt).
  while (left > 0) {
    let best = "", score = -1;
    for (const c of countries) {
      const q = quota.get(c), n = groups.get(c).length;
      if (q >= n) continue;
      const s = n / (q + 1);
      if (s > score) [best, score] = [c, s];
    }
    if (score < 0) break;
    quota.set(best, quota.get(best) + 1);
    left--;
  }
  const out = [];
  for (const c of countries) {
    const q = quota.get(c);
    if (!q) continue;
    const g = groups.get(c);
    const window = g.slice(0, Math.min(g.length, q * SLICE_WINDOW));
    // Each server's place, moved back by up to the window's length.
    const ranked = window
      .map((x, k) => ({ ...x, at: k + (fnv(seed + "|" + x.e.link) / 4294967296) * window.length }))
      .sort((a, b) => a.at - b.at);
    // In turns over the ways through, the best of each first.
    const byCarrier = new Map();
    for (const x of ranked) {
      const k = carrierOf(x.e.link);
      if (!byCarrier.has(k)) byCarrier.set(k, []);
      byCarrier.get(k).push(x);
    }
    const lanes = [...byCarrier.values()];
    for (let round = 0, taken = 0; taken < q; round++) {
      for (const lane of lanes) {
        if (taken < q && round < lane.length) {
          out.push(lane[round]);
          taken++;
        }
      }
    }
  }
  return out.sort((a, b) => a.i - b.i).map((x) => x.e);
}

// What gets through when only approved sites do (an allowlist): servers
// reached through the CDN, on Cloudflare's own addresses. A slice of the
// best-tested servers holds few of them, so each account also gets its own
// share of them from the wider lists, the same all day.
export const SLICE_RESIST = 60;
// Tested servers only: the collector connected through each and measured it.
const RESIST_LISTS = ["hot.txt"];

/** An account's share of the servers reached through the CDN, apart from those it already has. */
export async function resistantFor(env, userId, t, have, n = SLICE_RESIST) {
  const seen = new Set(have.map((e) => e.link));
  const cands = [];
  for (const name of RESIST_LISTS) {
    for (const e of parseList(await listFile(env, name))) {
      if (cands.length >= n * 8) break;
      if (e.country === "IR" || seen.has(e.link)) continue;
      const c = carrierOf(e.link);
      if (c !== "cdn" && c !== "grpc") continue;
      seen.add(e.link);
      cands.push(e);
    }
  }
  const pick = seededPick("resist|" + (await sliceSeed(env, userId, t)));
  const chosen = new Set();
  const want = Math.min(n, cands.length);
  while (chosen.size < want) chosen.add(Math.floor(pick() * cands.length) % cands.length);
  return cands.filter((_, i) => chosen.has(i));
}

/** An account's slice of a pool for the day of t (slice, sliceSeed). */
export async function sliceFor(env, userId, entries, t, cap = SLICE_MAX, base = SLICE_BASE) {
  return slice(entries, await sliceSeed(env, userId, t), cap, base);
}

/**
 * How many servers each plan gets: a higher plan more of the best-tested
 * ones (base, growing to max with many countries) and more of those reached
 * through the CDN (resist). Even the highest gets a small part of what the
 * collector holds, and never more than SLICE_SHARE of the pool, so no
 * account takes the best-tested list whole.
 */
export const SLICE_TIERS = {
  bronze: { base: 60, max: 120, resist: 40 },
  silver: { base: 90, max: 160, resist: 60 },
  gold: { base: 120, max: 200, resist: 80 },
  vip: { base: 160, max: 260, resist: 100 },
};
export const SLICE_SHARE = 0.7;

/** The sizes of a plan's slice of a pool of n servers (SLICE_TIERS). */
export function sliceSize(tier, n) {
  const s = SLICE_TIERS[tier] || SLICE_TIERS.bronze;
  const most = Math.max(SLICE_BASE, Math.ceil(n * SLICE_SHARE));
  return { base: Math.min(s.base, most), max: Math.min(s.max, most), resist: s.resist };
}

/**
 * The links of an account's slice for the app: each named after its
 * country, so neither the servers' own names nor where they were found
 * reach the app (a link without a name keeps none).
 */
export function appLinks(entries) {
  const n = new Map();
  return entries.map((e) => {
    if (!e.link.startsWith("vmess://") && !e.link.includes("#")) return e.link;
    const c = e.country || "";
    n.set(c, (n.get(c) || 0) + 1);
    const where = c ? `${flag(c)} ${COUNTRIES[c] || c}` : "Server";
    return rename(e.link, `${where} ${n.get(c)}`);
  });
}

// How many clean Cloudflare addresses a subscription carries the edge on.
const EDGE_ADDRS = 3;

/** Clean Cloudflare addresses found open from inside the country (reach.json), a sample of n. */
export async function cleanEdges(env, n = EDGE_ADDRS, pick = Math.random) {
  try {
    const r = JSON.parse((await listFile(env, "reach.json")) || "{}");
    const all = Array.isArray(r.cloudflare) ? r.cloudflare.filter((a) => typeof a === "string" && /^[0-9.]+$/.test(a)) : [];
    const out = [];
    while (out.length < n && all.length > 0) out.push(all.splice(Math.floor(pick() * all.length) % all.length, 1)[0]);
    return out;
  } catch {
    return [];
  }
}

/** The edge's links for an account that may connect on a plan with the edge, by name and by clean addresses. */
export async function edgeFor(env, account) {
  if (!account.allowed || !(account.features || []).includes("edge")) return [];
  return edgeLinks(env.EDGE_HOST, env.EDGE_UUID, await cleanEdges(env));
}

export { accountOf };

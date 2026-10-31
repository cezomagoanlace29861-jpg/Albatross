// The account service: the apps sign in and report use here, users see and
// change their account on the website, the administrator manages everyone.
import {
  DAY, DEFAULT_TIER, DEVICE_ACTIVE, FEATURES, SUB_ACTIVE, TIERS, accountOf, checkPassword, checkUsername, edgeLinks, hashPassword, maxDevices, planFeatures,
  newest, versionLess, randomPassword, rollPeriod, safeEqual, sign, tag, tierForQuota, tierOf, verify, verifyPassword,
} from "./lib.js";
import { LIST_PATH, MAX_LIST, pullFile, putList, serveFile, serveList } from "./files.js";
import { armor, httpsRedirect, securityTxt } from "./headers.js";
import { singBoxConfig, wantsSingBox } from "./singbox.js";
import { appLinks, appPool, provenStatus, resistantFor, sliceSize, build as buildSub, countriesOf, dayPick, edgeFor, headers as subHeaders, listUpdated, readSubToken, SLICE_COUNTRY, SLICE_MAX, sliceFor, storedListKeys, subPool, subToken } from "./sub.js";
import { chat as supportChat, close as supportClose, withCors as supportCors } from "./support.js";
import { refreshRates } from "./fx.js";
import { MAX_GB, MIN_GB, MONTHS, SALES_DEFAULTS, SALES_RANGE, catalog as salesCatalog, config as salesConfig, parseSteps } from "./sales.js";
import { SWEEP_CRON, check as telegramCheck, connect as telegramConnect, disconnect as telegramDisconnect, heal as telegramHeal, repair as telegramRepair, sweep as telegramSweep, webhook as telegramWebhook } from "./telegram.js";

const SESSION = 30 * DAY; // an app or browser session
const ADMIN_SESSION = 8 * 3600;
const ADMIN_ALL = "admin:all"; // the wrong administrator sign-ins of every address, together
const ADMIN_ALL_MAX = 200;
const MAX_BODY = 8 * 1024;

const DEFAULTS = {
  signup_open: "0",
  default_quota_bytes: String(5 * 1024 ** 3),
  default_max_seconds: "0",
  default_valid_days: "0",
  default_reset_days: "0", // allowances renew only when the admin recharges them
  default_tier: "auto", // the plan sign-ups get: by their allowance, or a named plan
  plan_features: "", // which plan brings each feature, where it differs from FEATURES (JSON)
  download_windows: "",
  download_android: "",
  support_notify_to: "", // where the support chat's summaries are mailed
  support_mail_status: "", // what the last try to mail one came to (written by the service, never by the panel)
  telegram_bot_username: "", // the support bot's name, once its token is set (read-only here)
  telegram_owner_chat: "", // the team's chat, once paired with /pair (read-only here)
  ...SALES_DEFAULTS, // selling data with Telegram Stars (sales.js)
};

class HttpError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

const json = (data, status = 200) =>
  new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff" },
  });

const now = () => Math.floor(Date.now() / 1000);

/** The address to write to: the CONTACT_EMAIL variable when it holds one, else the project's own. */
const contactOf = (env) => (/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(env.CONTACT_EMAIL || "") ? env.CONTACT_EMAIL : "support@albatrossvpn.online");

function randomUsernameSuffix() {
  const range = 1_000_000;
  const limit = Math.floor(0x1_0000_0000 / range) * range;
  const value = new Uint32Array(1);
  do {
    crypto.getRandomValues(value);
  } while (value[0] >= limit);
  return String(value[0] % range).padStart(6, "0");
}

export default {
  // Every minute: the Telegram bot's chats that went quiet.
  // Hourly: users' reports about servers, summed for the collector, and a look at the bot's webhook.
  async scheduled(event, env, ctx) {
    if (event.cron === SWEEP_CRON) ctx.waitUntil(telegramSweep(env, supportKit(env)));
    else ctx.waitUntil(Promise.allSettled([publishCrowdServers(env), telegramHeal(env), refreshRates(env)]));
  },
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    // What the service answers itself carries the security headers; the
    // static pages carry theirs from public/_headers. Nothing is served over
    // plain HTTP (the pages that matter reach this code first, wrangler.toml).
    const insecure = httpsRedirect(request);
    if (insecure) return armor(insecure);
    if (url.pathname === "/.well-known/security.txt") return armor(securityTxt(request, contactOf(env)));
    if (url.pathname.startsWith("/list/")) return armor(await mirror(request, url, env));
    if (url.pathname.startsWith("/dl/")) return armor(await download(request, url, env));
    if (url.pathname.startsWith("/sub/")) return armor(await subscription(request, url, env));
    if (!url.pathname.startsWith("/api/")) return env.ASSETS.fetch(request);
    // The websites' support chat is read from the website's own origin.
    const cors = url.pathname.startsWith("/api/v1/support/") ? (r) => supportCors(request, r) : (r) => r;
    try {
      if (!env.TOKEN_SECRET || env.TOKEN_SECRET.length < 24) throw new HttpError(503, "The service is not configured yet.");
      return armor(cors(await route(request, env, url, ctx)));
    } catch (e) {
      if (e instanceof HttpError) return armor(cors(json({ error: e.message }, e.status)));
      console.error(e && e.stack ? e.stack : e);
      return armor(cors(json({ error: "Something went wrong. Try again." }, 500)));
    }
  },
};

async function route(request, env, url, ctx) {
  const path = url.pathname.replace(/^\/api\/v1\/?/, "");
  const key = `${request.method} ${path}`;
  const admin = path.match(/^admin\/users\/(\d+)(?:\/([a-z-]+))?$/);

  switch (key) {
    case "GET public": return publicInfo(env);
    case "POST login": return login(request, env);
    case "POST signup": return signup(request, env);
    case "GET me": return me(request, env);
    case "GET edge": return edge(request, env);
    case "GET listkeys": return listKeys(request, env);
    case "GET servers": return servers(request, env);
    case "GET devices": return devicesList(request, env);
    case "GET dns/google.mobileconfig": return dnsProfile("google");
    case "GET dns/cloudflare.mobileconfig": return dnsProfile("cloudflare");
    case "GET sub": return subInfo(request, env, url);
    case "POST usage": return usage(request, env);
    case "POST password": return changePassword(request, env);
    case "POST admin/login": return adminLogin(request, env);
    case "GET admin/stats": return adminStats(request, env);
    case "GET admin/users": return adminList(request, env, url);
    case "POST admin/users": return adminCreate(request, env);
    case "POST admin/users/bulk": return adminBulk(request, env);
    case "GET admin/listkeys": return adminListKeys(request, env);
    case "GET admin/settings": return adminSettings(request, env);
    case "PUT admin/settings": return adminSaveSettings(request, env);
    case "POST admin/files": return adminPullFile(request, env);
    case "POST stats": return crowdReport(request, env);
    case "POST admin/crowd/publish": return adminPublishCrowd(request, env);
    case "GET admin/diag": return adminDiag(request, env, url);
    case "GET stats": return crowdSummary(request, env, url);
    case "POST telegram": return telegramWebhook(request, env, supportKit(env), ctx);
    case "GET support/info": return supportInfo(env);
    case "POST support/chat": return supportChat(request, env, supportKit(env));
    case "POST support/close": return supportClose(request, env, supportKit(env), ctx);
    case "OPTIONS support/chat":
    case "OPTIONS support/close": return new Response(null, { status: 204 });
  }
  const list = path.match(/^admin\/list\/(.+)$/);
  if (list && request.method === "PUT") return adminPutList(request, env, list[1]);
  const dev = path.match(/^devices\/([a-z0-9]{1,64})$/);
  if (dev && request.method === "DELETE") return deviceRemove(request, env, dev[1]);
  if (admin) {
    const id = Number(admin[1]);
    if (request.method === "PATCH" && !admin[2]) return adminUpdate(request, env, id);
    if (request.method === "DELETE" && !admin[2]) return adminDelete(request, env, id);
  }
  throw new HttpError(404, "Not found.");
}

// ---------- Helpers ----------

async function body(request) {
  // A body announced as too large is refused before it is read.
  if (Number(request.headers.get("Content-Length") || 0) > MAX_BODY) throw new HttpError(413, "Request too large.");
  const text = await request.text();
  if (text.length > MAX_BODY) throw new HttpError(413, "Request too large.");
  try {
    const v = text ? JSON.parse(text) : {};
    if (v && typeof v === "object" && !Array.isArray(v)) return v;
  } catch {}
  throw new HttpError(400, "Bad request.");
}

async function settings(env) {
  const out = { ...DEFAULTS };
  const { results } = await env.DB.prepare("SELECT key, value FROM settings").all();
  for (const r of results) if (r.key in DEFAULTS) out[r.key] = r.value;
  return out;
}

// Which plan brings each feature (planFeatures), read at most once a
// minute per isolate: every app asks for its account every minute.
let plansCache = { at: 0, plans: null };
async function plansOf(env) {
  if (!plansCache.plans || Date.now() - plansCache.at > 60_000) {
    const row = await env.DB.prepare("SELECT value FROM settings WHERE key = 'plan_features'").first();
    plansCache = { at: Date.now(), plans: planFeatures(row ? row.value : "") };
  }
  return plansCache.plans;
}

// An IPv6 client is usually given a whole /64, so the limits count the
// prefix as one address; otherwise each few guesses could take a new one.
function clientAddress(request) {
  const ip = (request.headers.get("CF-Connecting-IP") || "unknown").toLowerCase();
  const mapped = /^::ffff:(\d+\.\d+\.\d+\.\d+)$/.exec(ip);
  if (mapped) return mapped[1];
  const halves = ip.split("::");
  if (!ip.includes(":") || halves.length > 2) return ip;
  const head = halves[0] ? halves[0].split(":") : [];
  const tail = halves[1] ? halves[1].split(":") : [];
  const groups = halves.length === 1 ? head : [...head, ...Array(Math.max(8 - head.length - tail.length, 0)).fill("0"), ...tail];
  const prefix = groups.slice(0, 4);
  if (groups.length !== 8 || !prefix.every((g) => /^[0-9a-f]{1,4}$/.test(g))) return ip;
  return prefix.map((g) => parseInt(g, 16).toString(16)).join(":") + "::/64";
}

const clientTag = (request, env) => tag(env.TOKEN_SECRET, clientAddress(request));

/** True when `key` has used up `max` attempts within the window. */
async function blocked(env, key, max, window) {
  const row = await env.DB.prepare("SELECT count, since FROM attempts WHERE key = ?1").bind(key).first();
  return !!row && now() - row.since <= window && row.count >= max;
}

async function count(env, key, window) {
  const t = now();
  await env.DB.batch([
    env.DB.prepare(
      `INSERT INTO attempts (key, count, since) VALUES (?1, 1, ?2)
       ON CONFLICT (key) DO UPDATE SET
         count = CASE WHEN ?2 - since > ?3 THEN 1 ELSE count + 1 END,
         since = CASE WHEN ?2 - since > ?3 THEN ?2 ELSE since END`,
    ).bind(key, t, window),
    // Old entries are dropped as new ones come in.
    env.DB.prepare("DELETE FROM attempts WHERE since < ?1").bind(t - DAY),
  ]);
}

const forget = (env, key) => env.DB.prepare("DELETE FROM attempts WHERE key = ?1").bind(key).run();

/** GET /support/info: what the websites' chat shows besides itself (the Telegram bot, once there is one). */
async function supportInfo(env) {
  const s = await settings(env);
  return json({ telegram: s.telegram_bot_username || "", stars: starsInfo(s, await salesConfig(env)) });
}

// What the support chat (support.js) takes from here: answers, rate limits,
// and who is signed in, for the mail to the owner.
const supportKit = (env) => ({
  json,
  blocked,
  count,
  tag: (request) => clientTag(request, env),
  account: async (request) => {
    if (!request.headers.get("Authorization")) return "";
    const { user } = await authUser(request, env);
    return `${user.username} (${tierOf(user.tier)})`;
  },
});

const tooMany = () => new HttpError(429, "Too many attempts. Wait a few minutes and try again.");

/** Rate-limit keys for checking the password of `name` from this request's address. */
async function passwordKeys(request, env, name) {
  const t = await clientTag(request, env);
  return { ip: "login:ip:" + t, who: "login:user:" + name, from: "login:from:" + t + ":" + name };
}

// Failures from all addresses lock a username outright only far above what
// typos make, or anyone who knows it could keep its owner out cheaply; that
// cap bounds a guesser with very many addresses. Below it, while many
// addresses guess at it, each one that already failed at it gets only a few
// tries; one that has not still signs in with the right password.
async function passwordBlocked(env, k) {
  if ((await blocked(env, k.ip, 40, 900)) || (await blocked(env, k.from, 8, 900)) || (await blocked(env, k.who, 100, 900))) return true;
  return (await blocked(env, k.who, 8, 900)) && (await blocked(env, k.from, 3, 900));
}

async function passwordFailed(env, k) {
  await count(env, k.ip, 900);
  await count(env, k.who, 900);
  await count(env, k.from, 900);
}

async function userByName(env, name) {
  return env.DB.prepare("SELECT * FROM users WHERE username = ?1").bind(String(name).trim().toLowerCase()).first();
}

const userById = (env, id) => env.DB.prepare("SELECT * FROM users WHERE id = ?1").bind(id).first();

const session = (env, user, ttl = SESSION) =>
  sign({ u: user.id, v: user.token_version, exp: now() + ttl }, env.TOKEN_SECRET);

async function authUser(request, env) {
  const m = /^Bearer (.+)$/.exec(request.headers.get("Authorization") || "");
  const p = m && (await verify(m[1], env.TOKEN_SECRET, now()));
  const user = p && p.u && (await userById(env, p.u));
  if (!user || user.token_version !== p.v) throw new HttpError(401, "Sign in again.");
  return { user, token: m[1], payload: p };
}

async function authAdmin(request, env) {
  const m = /^Bearer (.+)$/.exec(request.headers.get("Authorization") || "");
  const p = m && (await verify(m[1], env.TOKEN_SECRET, now()));
  if (!p || p.adm !== 1) throw new HttpError(401, "Sign in again.");
}

// A fixed hash to spend the same time on an unknown username as on a known one.
const DUMMY = "pbkdf2$100000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";

// ---------- Public ----------

async function publicInfo(env) {
  const s = await settings(env);
  const shop = await salesConfig(env);
  return json({
    name: env.SITE_NAME || "Albatross",
    company: env.COMPANY_NAME || "",
    address: env.COMPANY_ADDRESS || "",
    contact: env.CONTACT_EMAIL || "",
    // Where VIP accounts reach support directly (a chat link), besides the
    // contact address.
    support_url: env.SUPPORT_URL || (s.telegram_bot_username ? `https://t.me/${s.telegram_bot_username}` : ""),
    signup_open: s.signup_open === "1",
    turnstile_sitekey: env.TURNSTILE_SITEKEY || "",
    downloads: { windows: s.download_windows, android: s.download_android },
    tiers: TIERS,
    features: FEATURES,
    plans: planFeatures(s.plan_features),
    stars: starsInfo(s, shop),
  });
}

/** Data on sale in the Telegram bot, for the websites' "how to buy": null while it is not (or there is no bot to sell in). */
const starsInfo = (s, shop) =>
  shop.enabled && s.telegram_bot_username
    ? {
        bot: s.telegram_bot_username, days: shop.days,
        packages: salesCatalog(shop).map(({ gb, stars, plan, devices, save }) => ({ gb, stars, plan, devices, save })),
        // A plan of one's own: data a month within these limits, for these months, less these discounts.
        custom: { min_gb: MIN_GB, max_gb: MAX_GB, months: MONTHS, size_discounts: shop.sizeSteps, month_discounts: shop.monthSteps },
      }
    : null;

// ---------- Encrypted DNS for Apple devices ----------
//
// On some networks the resolver answers for this service's names with a
// lie, so the account page and the subscription never load there. A
// configuration profile that sets DNS over HTTPS for the whole device
// goes around that; Safari installs it from these addresses.

const DNS_PROVIDERS = {
  google: { name: "Google", url: "https://dns.google/dns-query", addrs: ["8.8.8.8", "8.8.4.4", "2001:4860:4860::8888", "2001:4860:4860::8844"], id: "google" },
  cloudflare: { name: "Cloudflare", url: "https://cloudflare-dns.com/dns-query", addrs: ["1.1.1.1", "1.0.0.1", "2606:4700:4700::1111", "2606:4700:4700::1001"], id: "cloudflare" },
};

function dnsProfile(which) {
  const p = DNS_PROVIDERS[which];
  if (!p) throw new HttpError(404, "Not found.");
  const xml = (s) => String(s).replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c]));
  const body = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>PayloadContent</key>
  <array>
    <dict>
      <key>DNSSettings</key>
      <dict>
        <key>DNSProtocol</key>
        <string>HTTPS</string>
        <key>ServerURL</key>
        <string>${xml(p.url)}</string>
        <key>ServerAddresses</key>
        <array>
${p.addrs.map((a) => `          <string>${a}</string>`).join("\n")}
        </array>
      </dict>
      <key>PayloadDescription</key>
      <string>Encrypted DNS (${xml(p.name)}) for every network</string>
      <key>PayloadDisplayName</key>
      <string>Encrypted DNS: ${xml(p.name)}</string>
      <key>PayloadIdentifier</key>
      <string>online.albatrossvpn.dns.${p.id}.settings</string>
      <key>PayloadType</key>
      <string>com.apple.dnsSettings.managed</string>
      <key>PayloadUUID</key>
      <string>a1b2c3d4-0000-4000-8000-${p.id === "google" ? "000000000001" : "000000000002"}</string>
      <key>PayloadVersion</key>
      <integer>1</integer>
      <key>ProhibitDisablement</key>
      <false/>
    </dict>
  </array>
  <key>PayloadDescription</key>
  <string>Looks names up over an encrypted connection to ${xml(p.name)}, so a network's resolver cannot answer with lies. Remove it any time in Settings, General, VPN &amp; Device Management.</string>
  <key>PayloadDisplayName</key>
  <string>Encrypted DNS (${xml(p.name)})</string>
  <key>PayloadIdentifier</key>
  <string>online.albatrossvpn.dns.${p.id}</string>
  <key>PayloadRemovalDisallowed</key>
  <false/>
  <key>PayloadType</key>
  <string>Configuration</string>
  <key>PayloadUUID</key>
  <string>a1b2c3d4-0000-4000-8000-${p.id === "google" ? "000000000011" : "000000000012"}</string>
  <key>PayloadVersion</key>
  <integer>1</integer>
</dict>
</plist>
`;
  return new Response(body, {
    status: 200,
    headers: {
      "Content-Type": "application/x-apple-aspen-config; charset=utf-8",
      "Content-Disposition": `attachment; filename="encrypted-dns-${p.id}.mobileconfig"`,
      "Cache-Control": "public, max-age=86400",
      "X-Content-Type-Options": "nosniff",
    },
  });
}

// ---------- Devices ----------
//
// An account's plan says how many devices may be connected at once. Each
// copy of the app names itself with a random number kept on the device
// (X-Albatross-Device) and a word for what it is (X-Albatross-Device-Name).
// A usage report, sent every minute while connected, claims one of the
// account's slots for DEVICE_ACTIVE; a report that finds the other slots
// taken is answered "devices" and the app disconnects. Signing in and
// looking at the account claim nothing.

/** The device an app's call names, or null for the website. */
function deviceOf(request) {
  if (request.headers.get("X-Albatross-Client") === "web") return null;
  const id = (request.headers.get("X-Albatross-Device") || "").trim().toLowerCase();
  const name = (request.headers.get("X-Albatross-Device-Name") || "").replace(/[^\x20-\x7e]/g, "").trim().slice(0, 40);
  // A copy from before devices were counted (1.0.0) is one shared device.
  if (!/^[0-9a-f]{16,64}$/.test(id)) return { id: "app", name: "" };
  return { id, name };
}

// The names shown for the devices that have none of their own.
const DEVICE_NAMES = { app: "Albatross app (older version)", sub: "iPhone, iPad and Mac (subscription)" };
const deviceName = (d) => d.name || DEVICE_NAMES[d.device_id ?? d.id] || "Device";

/** How many of the account's devices other than `id` hold a slot now. */
async function busyDevices(env, user, id, t) {
  const r = await env.DB.prepare("SELECT COUNT(*) AS n FROM devices WHERE user_id = ?1 AND device_id != ?2 AND active_until > ?3")
    .bind(user.id, id, t).first();
  return r.n;
}

const upsertDevice = (env, user, d, t, until) => env.DB.prepare(
  `INSERT INTO devices (user_id, device_id, name, first_seen, last_seen, active_until) VALUES (?1, ?2, ?3, ?4, ?4, ?5)
   ON CONFLICT (user_id, device_id) DO UPDATE SET last_seen = ?4, name = CASE WHEN ?3 = '' THEN name ELSE ?3 END,
     active_until = CASE WHEN ?5 < 0 THEN active_until ELSE ?5 END`,
).bind(user.id, d.id, d.name, t, until).run();

/** The store, for the apps' own "Buy" (starsInfo): where and what an account can buy; null while nothing is on sale. */
async function storeOf(env) {
  return starsInfo(await settings(env), await salesConfig(env));
}

/** An account as an app is told of it: with the store, so the app can offer its packages. */
const forApp = async (env, a) => ({ ...a, store: await storeOf(env) });

/**
 * The account as a caller sees it. A device is told whether a slot is
 * free for it (and remembered, holding nothing); the website is told how
 * many devices hold one.
 */
async function viewOf(env, user, d, t) {
  const plans = await plansOf(env);
  if (!d) return accountOf(user, t, { active: await busyDevices(env, user, "", t), plans });
  await upsertDevice(env, user, d, t, -1);
  return accountOf(user, t, { others: await busyDevices(env, user, d.id, t), plans });
}

/**
 * A device asks to hold one of its account's slots for `hold` seconds. It
 * may while fewer than the plan's number of other devices hold one; a
 * device that may not connect gives up the slot it held, so it never
 * counts the devices that stay out.
 */
async function claimSlot(env, user, d, t, hold = DEVICE_ACTIVE) {
  const a = accountOf(user, t, { others: await busyDevices(env, user, d.id, t), plans: await plansOf(env) });
  await upsertDevice(env, user, d, t, a.allowed ? t + hold : 0);
  return a;
}

/** The devices this account has been used on, those holding a slot first. */
async function devicesList(request, env) {
  const { user } = await authUser(request, env);
  const t = now();
  const { results } = await env.DB.prepare(
    "SELECT device_id, name, first_seen, last_seen, active_until FROM devices WHERE user_id = ?1 ORDER BY (active_until > ?2) DESC, last_seen DESC LIMIT 100",
  ).bind(user.id, t).all();
  return json({
    tier: tierOf(user.tier),
    max_devices: maxDevices(user),
    devices: results.map((d) => ({ id: d.device_id, name: deviceName(d), first_seen: d.first_seen, last_seen: d.last_seen, active: d.active_until > t })),
  });
}

/** Forgets a device; the slot it held is free at once. */
async function deviceRemove(request, env, id) {
  const { user } = await authUser(request, env);
  const r = await env.DB.prepare("DELETE FROM devices WHERE user_id = ?1 AND device_id = ?2").bind(user.id, id).run();
  if (!r.meta.changes) throw new HttpError(404, "No such device.");
  return json({ ok: true });
}

const forgetDevices = (env, id) => env.DB.prepare("DELETE FROM devices WHERE user_id = ?1").bind(id).run();

// ---------- Users ----------

async function login(request, env) {
  await requireCurrentClient(request, env);
  const { username, password } = await body(request);
  if (typeof username !== "string" || typeof password !== "string" || password.length > 200 || username.length > 64) {
    throw new HttpError(400, "Enter your username and password.");
  }
  const name = username.trim().toLowerCase();
  const k = await passwordKeys(request, env, name);
  if (await passwordBlocked(env, k)) throw tooMany();

  const user = await userByName(env, name);
  const ok = await verifyPassword(password, user ? user.pass_hash : DUMMY);
  if (!user || !ok) {
    await passwordFailed(env, k);
    throw new HttpError(401, "Wrong username or password.");
  }
  // The username's count stays, so the other addresses guessing at it stay slowed.
  await forget(env, k.from);
  return json({ token: await session(env, user), account: await forApp(env, await viewOf(env, user, deviceOf(request), now())) });
}

async function verifyTurnstile(request, env, token) {
  if (!env.TURNSTILE_SECRET) return;
  const form = new FormData();
  form.append("secret", env.TURNSTILE_SECRET);
  form.append("response", String(token || ""));
  const ip = request.headers.get("CF-Connecting-IP");
  if (ip) form.append("remoteip", ip);
  const res = await fetch("https://challenges.cloudflare.com/turnstile/v0/siteverify", { method: "POST", body: form });
  const out = await res.json().catch(() => ({}));
  if (!out.success) throw new HttpError(400, "Verification failed. Reload the page and try again.");
}

async function signup(request, env) {
  const s = await settings(env);
  if (s.signup_open !== "1") throw new HttpError(403, "Sign-ups are closed.");
  const { username, password, turnstile } = await body(request);
  const ip = "signup:ip:" + (await clientTag(request, env));
  if (await blocked(env, ip, 5, 3600)) throw tooMany();
  await verifyTurnstile(request, env, turnstile);
  if (!checkUsername(username)) throw new HttpError(400, "Choose a username of 3 to 32 letters, digits, dots, dashes or underscores.");
  if (!checkPassword(password)) throw new HttpError(400, "Choose a password of at least 8 characters.");

  // Every attempt counts, the ones turned away as taken too: otherwise the
  // answers could be used to learn which usernames exist, without limit.
  await count(env, ip, 3600);
  const t = now();
  const days = Number(s.default_valid_days);
  try {
    await env.DB.prepare(
      `INSERT INTO users (username, pass_hash, created_at, quota_bytes, max_seconds, expires_at, reset_days, period_start, tier)
       VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?3, ?8)`,
    ).bind(username.trim().toLowerCase(), await hashPassword(password), t, Number(s.default_quota_bytes), Number(s.default_max_seconds),
      days > 0 ? t + days * DAY : 0, Number(s.default_reset_days),
      s.default_tier === "auto" || !s.default_tier ? tierForQuota(Number(s.default_quota_bytes)) : tierOf(s.default_tier)).run();
  } catch (e) {
    if (/UNIQUE/i.test(String(e))) throw new HttpError(409, "That username is taken.");
    throw e;
  }
  const user = await userByName(env, username);
  return json({ token: await session(env, user), account: accountOf(user, t, { plans: await plansOf(env) }) }, 201);
}

async function me(request, env) {
  await requireCurrentClient(request, env);
  const { user } = await authUser(request, env);
  const account = await viewOf(env, user, deviceOf(request), now());
  return json({ account, created_at: user.created_at, total_bytes: user.total_bytes, total_seconds: user.total_seconds });
}

/** The edge's links for the signed-in user (edge/README.md); none while the account may not connect, or on a plan without the edge. */
async function edge(request, env) {
  await requireCurrentClient(request, env);
  const { user } = await authUser(request, env);
  const a = await viewOf(env, user, deviceOf(request), now());
  // The edge comes with a plan ("edge"): the others are told why it is not theirs.
  if (a.allowed && !a.features.includes("edge")) return json({ links: [], reason: "plan" });
  return json({ links: a.allowed ? edgeLinks(env.EDGE_HOST, env.EDGE_UUID) : [], reason: a.reason });
}

// How many of the collector's keys a signed-in app gets: enough for the
// lists a cache may still serve (a collection every twenty minutes).
const LIST_KEYS_GIVEN = 12;

/**
 * The keys that open the sealed lists of the project's own servers
 * (premium, cmd/collect -keys-out), for a signed-in user who may connect on
 * a plan that has them: the newest first. The keys of the collector's
 * lists of what it collected are given to no app: each account gets a
 * slice of those servers instead (GET servers), so no account, and no
 * copy of the app with its checks cut out, can read the whole collection.
 */
async function listKeys(request, env) {
  await requireCurrentClient(request, env);
  const { user } = await authUser(request, env);
  const a = await viewOf(env, user, deviceOf(request), now());
  if (!a.allowed) return json({ keys: [], reason: a.reason });
  const { keys, premium } = await recentListKeys(env);
  const own = a.features.includes("own") ? premium : [];
  // Copies older than 1.3.0 read only the collector's sealed lists (they
  // know no slice), so while they may still connect they get those keys;
  // 1.3.0 and later get their slice instead (GET servers).
  const version = request.headers.get("X-Albatross-Version") || OLD_CLIENT;
  const old = versionLess(version, "1.3.0");
  return json({ keys: (old ? [...keys, ...own] : own).sort((x, y) => y.id - x.id), reason: "" });
}

// How often an account may ask for its slice in an hour: the app asks a
// few times a day; a script asking more learns nothing more anyway.
const SERVERS_PER_HOUR = 30;

/**
 * The signed-in account's slice of the best-tested servers (sub.js slice):
 * the same all day, another the next, each named after its country.
 */
async function servers(request, env) {
  await requireCurrentClient(request, env);
  const { user } = await authUser(request, env);
  const t = now();
  const a = await viewOf(env, user, deviceOf(request), t);
  if (!a.allowed) return json({ servers: [], reason: a.reason });
  const key = "servers:" + user.id;
  if (await blocked(env, key, SERVERS_PER_HOUR, 3600)) throw tooMany();
  await count(env, key, 3600);
  const entries = await appPool(env, dayPick(t));
  if (!entries.length) noProven();
  // A higher plan gets more servers (sub.js SLICE_TIERS).
  const size = sliceSize(a.tier, entries.length);
  const base = await sliceFor(env, user.id, entries, t, size.max, size.base);
  // Plus a share of the servers reached through the CDN, which are what
  // still gets out when the network lets only approved sites through.
  const resist = await resistantFor(env, user.id, t, base, size.resist);
  return json({ servers: appLinks([...base, ...resist]), reason: "" });
}

// An empty proven list leaves every account without the lists' servers:
// said in the Worker's log (wrangler tail), at most every ten minutes per
// isolate; the administrator's overview shows the count (adminStats).
let noProvenSaid = 0;
function noProven() {
  if (Date.now() - noProvenSaid < 600_000) return;
  noProvenSaid = Date.now();
  console.warn("servers: the proven list (list/servers/proven.txt) is missing or empty; accounts get none of the lists' servers");
}

/**
 * The same keys for the administrator: the tester inside the country
 * (deploy/tester) opens the sealed lists with them and seals the hot.txt it
 * publishes with the newest of `keys`, which never holds a premium key.
 */
async function adminListKeys(request, env) {
  await authAdmin(request, env);
  const { keys, premium } = await recentListKeys(env);
  return json({ keys, premium_keys: premium, reason: "" });
}

/** The newest stored list keys, the premium ones (the own servers' lists) apart. */
async function recentListKeys(env) {
  const all = (await storedListKeys(env))
    .filter((k) => Number.isSafeInteger(k.id) && typeof k.key === "string" && /^[0-9a-f]{64}$/.test(k.key))
    .sort((x, y) => y.id - x.id);
  const pick = (premium) => all.filter((k) => !!k.premium === premium).slice(0, LIST_KEYS_GIVEN).map(({ id, key }) => ({ id, key }));
  return { keys: pick(false), premium: pick(true) };
}

/**
 * What the account page needs for other devices (iPhone, iPad, Mac):
 * the subscription address and the countries on offer.
 */
async function subInfo(request, env, url) {
  const { user } = await authUser(request, env);
  const t = now();
  const a = await viewOf(env, user, null, t);
  const entries = await subPool(env, dayPick(t));
  return json({
    url: `${url.origin}/sub/${await subToken(env, user, t)}`,
    countries: countriesOf(entries),
    servers: (await sliceFor(env, user.id, entries, t, sliceSize(a.tier, entries.length).max, sliceSize(a.tier, entries.length).base)).length,
    edge: (await edgeFor(env, a)).length,
    brand: env.SITE_NAME || "Albatross",
  });
}

/** GET /sub/<token>?c=<country|edge|empty>: the subscription a client app downloads. */
async function subscription(request, url, env) {
  if (request.method !== "GET" && request.method !== "HEAD") return new Response("Method not allowed", { status: 405 });
  const t = now();
  const p = await readSubToken(env, url.pathname.slice("/sub/".length), t);
  const user = p && (await userById(env, p.u));
  if (!user || user.token_version !== p.v) return new Response("This subscription address is no longer valid. Sign in to your account for a new one.", { status: 401 });
  // The service never hears from these devices while they are connected,
  // so the subscription holds one slot for a while after each fetch.
  const a = await claimSlot(env, user, { id: "sub", name: "" }, t, SUB_ACTIVE);
  if (a.reason === "devices") {
    return new Response(`All ${a.max_devices} devices of this account are in use. Disconnect one, or remove it on your account page.`, { status: 403 });
  }
  if (!a.allowed) return new Response("This account cannot connect right now (" + a.reason + ").", { status: 403 });
  const choice = String(url.searchParams.get("c") || "").toUpperCase().replace("EDGE", "edge");
  if (choice && choice !== "edge" && !/^[A-Z]{2}$/.test(choice)) return new Response("Not found", { status: 404 });
  // One country's servers come with choosing a location, the edge's with the edge.
  const needs = choice === "edge" ? "edge" : choice ? "locations" : "";
  if (needs && !a.features.includes(needs)) {
    return new Response(`${FEATURES[needs].name} comes with the ${TIERS[a.plans[needs]].name} plan and above. The subscription for all servers works on every plan.`, { status: 403 });
  }
  const [entries, updated, edge] = await Promise.all([subPool(env, dayPick(t)), listUpdated(env), edgeFor(env, a)]);
  // The account's slice (sub.js slice), of the chosen country's servers
  // when there is a choice (fewer: SLICE_COUNTRY): the whole pool goes to no one.
  const chosen = choice === "edge" ? [] : choice ? entries.filter((e) => e.country === choice) : entries;
  const size = sliceSize(a.tier, entries.length);
  const [cap, base] = choice && choice !== "edge" ? [SLICE_COUNTRY, SLICE_COUNTRY] : [size.max, size.base];
  const sub = buildSub(await sliceFor(env, user.id, chosen, t, cap, base), choice, env.SITE_NAME || "Albatross", edge, updated);
  const h = subHeaders(sub.title, a);
  if (wantsSingBox(request, url)) {
    // The sing-box apps load a whole config, not a list of links.
    const config = await singBoxConfig(sub.servers);
    if (!config) return new Response("No server in this list runs on sing-box right now. Try again in a few minutes.", { status: 503 });
    h.set("Content-Type", "application/json; charset=utf-8");
    h.set("Content-Disposition", h.get("Content-Disposition").replace(/\.txt"$/, '.json"'));
    return new Response(request.method === "HEAD" ? null : JSON.stringify(config), { status: 200, headers: h });
  }
  return new Response(request.method === "HEAD" ? null : sub.body, { status: 200, headers: h });
}

const MAX_BYTES_PER_REPORT = 100 * 1024 ** 3;
const MAX_SECONDS_PER_REPORT = 7 * DAY;

/** The app reports data and time used since its last report and learns whether the user may go on. */
async function usage(request, env) {
  await requireCurrentClient(request, env);
  const { user, token, payload } = await authUser(request, env);
  const b = await body(request);
  const bytes = b.bytes ?? 0;
  const seconds = b.seconds ?? 0;
  if (!Number.isSafeInteger(bytes) || !Number.isSafeInteger(seconds) || bytes < 0 || seconds < 0 ||
      bytes > MAX_BYTES_PER_REPORT || seconds > MAX_SECONDS_PER_REPORT) {
    throw new HttpError(400, "Bad usage report.");
  }
  const t = now();
  const p = rollPeriod(user, t);
  if (p.rolled) {
    // A new period: only one of several devices reporting at once wins this update.
    await env.DB.prepare("UPDATE users SET period_start = ?1, used_bytes = 0, used_seconds = 0 WHERE id = ?2 AND period_start = ?3")
      .bind(p.period_start, user.id, user.period_start).run();
  }
  // A report the app sends again (its answer was lost on the way) is
  // counted once: its id, when it carries one, is remembered for a few days.
  let apply = true;
  const id = typeof b.id === "string" && /^[0-9a-f]{8,64}$/.test(b.id) ? user.id + ":" + b.id : "";
  if (id) {
    const day = Math.floor(t / DAY);
    const r = await env.DB.prepare("INSERT OR IGNORE INTO usage_ids (id, day) VALUES (?1, ?2)").bind(id, day).run();
    apply = r.meta.changes > 0;
    if (apply && Math.random() < 0.01) await env.DB.prepare("DELETE FROM usage_ids WHERE day < ?1").bind(day - 7).run();
  }
  if (apply) {
    await env.DB.prepare(
      `UPDATE users SET used_bytes = used_bytes + ?1, used_seconds = used_seconds + ?2,
         total_bytes = total_bytes + ?1, total_seconds = total_seconds + ?2, last_seen = ?3 WHERE id = ?4`,
    ).bind(bytes, seconds, t, user.id).run();
  }
  const fresh = await userById(env, user.id);
  // Sessions are renewed while they are in use.
  const renewed = payload.exp - t < SESSION / 2 ? await session(env, fresh) : token;
  const d = deviceOf(request);
  return json({ token: renewed, account: await forApp(env, d ? await claimSlot(env, fresh, d, t) : accountOf(fresh, t, { plans: await plansOf(env) })) });
}

async function changePassword(request, env) {
  await requireCurrentClient(request, env);
  const { user } = await authUser(request, env);
  const { old_password, new_password } = await body(request);
  const k = await passwordKeys(request, env, user.username.toLowerCase());
  if (await passwordBlocked(env, k)) throw tooMany();
  if (typeof old_password !== "string" || !(await verifyPassword(old_password, user.pass_hash))) {
    await passwordFailed(env, k);
    throw new HttpError(403, "The current password is wrong.");
  }
  if (!checkPassword(new_password)) throw new HttpError(400, "Choose a password of at least 8 characters.");
  // Raising the version signs out every other device.
  await env.DB.prepare("UPDATE users SET pass_hash = ?1, token_version = token_version + 1 WHERE id = ?2")
    .bind(await hashPassword(new_password), user.id).run();
  await forgetDevices(env, user.id);
  const fresh = await userById(env, user.id);
  return json({ token: await session(env, fresh), account: await forApp(env, await viewOf(env, fresh, deviceOf(request), now())) });
}

// ---------- Administrator ----------

async function adminLogin(request, env) {
  if (!env.ADMIN_USER || !env.ADMIN_PASSWORD) throw new HttpError(503, "The administrator account is not set up.");
  const { username, password } = await body(request);
  const ip = "admin:ip:" + (await clientTag(request, env));
  // One address gets ten wrong tries; all addresses together get 200 (a
  // guesser with many addresses is held to a total a dictionary cannot get
  // far on in a day, and a right password from a quiet address still waits
  // for the window rather than hand the flood a way to tell it apart).
  if ((await blocked(env, ip, 10, 900)) || (await blocked(env, ADMIN_ALL, ADMIN_ALL_MAX, 900))) throw tooMany();
  // Both checks always run, so the answer does not say which one was wrong.
  const [a, b] = await Promise.all([safeEqual(username ?? "", env.ADMIN_USER), safeEqual(password ?? "", env.ADMIN_PASSWORD)]);
  if (!(a && b)) {
    await count(env, ip, 900);
    await count(env, ADMIN_ALL, 900);
    throw new HttpError(401, "Wrong username or password.");
  }
  await forget(env, ip);
  return json({ token: await sign({ adm: 1, exp: now() + ADMIN_SESSION }, env.TOKEN_SECRET) });
}

/** The administrator (the release workflow) stores a release file from GitHub in the project's own storage. */
async function adminPullFile(request, env) {
  await authAdmin(request, env);
  if (!env.FILES) throw new HttpError(503, "No file storage is bound.");
  const { name, url, force } = await body(request);
  try {
    return json(await pullFile(env.FILES, String(name || ""), String(url || ""), { force: !!force }));
  } catch (e) {
    throw new HttpError(400, e.message);
  }
}

// Copies of the app send their version (X-Albatross-Version); those before
// 1.0.0 send none, so a request without it is taken to come from 0.4.0.
const OLD_CLIENT = "0.4.0";
let minClientCache = null; // { at, version }

/**
 * The oldest version that may still connect: min_version of the published
 * update manifest, or MIN_CLIENT (a variable set when the service is
 * deployed, deploy-backend.yml) when that is newer.
 */
async function minClient(env) {
  if (minClientCache && now() - minClientCache.at < 60) return newest(minClientCache.version, env.MIN_CLIENT);
  let version = "";
  try {
    const obj = env.FILES && (await env.FILES.get("list/main/update.json"));
    if (obj) version = String((await obj.json()).min_version || "");
  } catch {}
  minClientCache = { at: now(), version };
  return newest(version, env.MIN_CLIENT);
}


/**
 * Refuses copies of the app older than the update manifest's min_version,
 * so a version cut off by a forced update cannot sign in or go on even if
 * it ignores the manifest. The website's own pages (X-Albatross-Client:
 * web) are not apps and are always served.
 */
async function requireCurrentClient(request, env) {
  if (request.headers.get("X-Albatross-Client") === "web") return;
  const min = await minClient(env);
  if (!min) return;
  const version = request.headers.get("X-Albatross-Version") || OLD_CLIENT;
  if (versionLess(version, min)) {
    throw new HttpError(426, "This version of Albatross no longer works. Install the new version from https://albatrossvpn.online");
  }
}

/** The administrator (CI) publishes one of the app's own files into the project's own storage. */
async function adminPutList(request, env, path) {
  await authAdmin(request, env);
  if (!env.FILES) throw new HttpError(503, "No file storage is bound.");
  if (!LIST_PATH.test(path)) throw new HttpError(404, "Not one of the app's files.");
  if (Number(request.headers.get("Content-Length") || 0) > MAX_LIST) throw new HttpError(413, "File too large.");
  try {
    const stored = await putList(env.FILES, path, await request.arrayBuffer());
    if (path === "main/update.json") minClientCache = null;
    return json(stored);
  } catch (e) {
    throw new HttpError(400, e.message);
  }
}

/** A user as the administrator sees them. */
function adminView(u, t, active = 0) {
  const a = accountOf(u, t, { active });
  return {
    id: u.id, username: u.username, created_at: u.created_at, disabled: !!u.disabled, note: u.note,
    tier: a.tier, max_devices: a.max_devices, devices: a.devices,
    quota_bytes: u.quota_bytes, max_seconds: u.max_seconds, expires_at: u.expires_at, reset_days: u.reset_days,
    used_bytes: a.used_bytes, used_seconds: a.used_seconds, resets_at: a.resets_at, status: a.allowed ? "active" : a.reason,
    total_bytes: u.total_bytes, total_seconds: u.total_seconds, last_seen: u.last_seen,
  };
}

async function adminStats(request, env) {
  await authAdmin(request, env);
  const t = now();
  const row = await env.DB.prepare(
    `SELECT COUNT(*) AS users, COALESCE(SUM(disabled), 0) AS disabled,
       COALESCE(SUM(CASE WHEN last_seen > ?1 THEN 1 ELSE 0 END), 0) AS active_day,
       COALESCE(SUM(CASE WHEN last_seen > ?2 THEN 1 ELSE 0 END), 0) AS active_month,
       COALESCE(SUM(total_bytes), 0) AS total_bytes, COALESCE(SUM(total_seconds), 0) AS total_seconds FROM users`,
  ).bind(t - DAY, t - 30 * DAY).first();
  // The servers accounts are given (sub.js PROVEN_LIST): none when it is empty.
  return json({ ...row, servers: await provenStatus(env).catch(() => null) });
}

async function adminList(request, env, url) {
  await authAdmin(request, env);
  const q = (url.searchParams.get("q") || "").trim().toLowerCase().replace(/[\\%_]/g, "\\$&");
  const limit = Math.min(Math.max(Number(url.searchParams.get("limit")) || 25, 1), 100);
  const offset = Math.max(Number(url.searchParams.get("offset")) || 0, 0);
  const like = `%${q}%`;
  const total = await env.DB.prepare("SELECT COUNT(*) AS n FROM users WHERE username LIKE ?1 ESCAPE '\\' OR note LIKE ?1 ESCAPE '\\'").bind(like).first();
  const { results } = await env.DB.prepare(
    "SELECT * FROM users WHERE username LIKE ?1 ESCAPE '\\' OR note LIKE ?1 ESCAPE '\\' ORDER BY id DESC LIMIT ?2 OFFSET ?3",
  ).bind(like, limit, offset).all();
  const t = now();
  const active = new Map();
  if (results.length) {
    const ids = results.map((u) => u.id);
    const rows = await env.DB.prepare(
      `SELECT user_id, COUNT(*) AS n FROM devices WHERE active_until > ?1 AND user_id IN (${ids.map((_, i) => `?${i + 2}`).join(",")}) GROUP BY user_id`,
    ).bind(t, ...ids).all();
    for (const r of rows.results) active.set(r.user_id, r.n);
  }
  return json({ total: total.n, users: results.map((u) => adminView(u, t, active.get(u.id) || 0)) });
}

/** Limits from a request, as the values stored. */
function limitsFrom(b, base = {}) {
  const out = {};
  const num = (k, max) => {
    if (b[k] === undefined) return;
    const v = Number(b[k]);
    if (!Number.isSafeInteger(v) || v < 0 || v > max) throw new HttpError(400, `Bad value for ${k}.`);
    out[k] = v;
  };
  num("quota_bytes", 1024 ** 5);
  num("max_seconds", 10 * 365 * DAY);
  num("expires_at", 4102444800);
  num("reset_days", 3650);
  if (b.note !== undefined) out.note = String(b.note).slice(0, 200);
  // The plan: as chosen, or ("" / auto / none given) the one the
  // allowance earns whenever the allowance is set.
  if (b.tier !== undefined && b.tier !== "" && b.tier !== "auto") {
    if (!Object.hasOwn(TIERS, b.tier)) throw new HttpError(400, `The plan is one of ${Object.keys(TIERS).join(", ")}.`);
    out.tier = b.tier;
  } else if (out.quota_bytes !== undefined) {
    out.tier = tierForQuota(out.quota_bytes);
  }
  return { ...base, ...out };
}

async function createUser(env, name, password, limits, t) {
  if (!checkUsername(name)) throw new HttpError(400, "Usernames have 3 to 32 letters, digits, dots, dashes or underscores.");
  try {
    await env.DB.prepare(
      `INSERT INTO users (username, pass_hash, created_at, quota_bytes, max_seconds, expires_at, reset_days, period_start, note, tier)
       VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?3, ?8, ?9)`,
    ).bind(name.trim().toLowerCase(), await hashPassword(password), t, limits.quota_bytes ?? 0, limits.max_seconds ?? 0,
      limits.expires_at ?? 0, limits.reset_days ?? 0, limits.note ?? "", limits.tier ?? tierForQuota(limits.quota_bytes ?? 0)).run();
  } catch (e) {
    if (/UNIQUE/i.test(String(e))) throw new HttpError(409, `The username "${name}" is taken.`);
    throw e;
  }
}

async function adminCreate(request, env) {
  await authAdmin(request, env);
  const b = await body(request);
  const password = b.password ? String(b.password) : randomPassword();
  if (!checkPassword(password)) throw new HttpError(400, "A password has at least 8 characters.");
  await createUser(env, String(b.username || ""), password, limitsFrom(b), now());
  const user = await userByName(env, b.username);
  return json({ user: adminView(user, now()), password }, 201);
}

async function adminBulk(request, env) {
  await authAdmin(request, env);
  const b = await body(request);
  const n = Number(b.count);
  const prefix = String(b.prefix || "user").toLowerCase();
  if (!Number.isInteger(n) || n < 1 || n > 200) throw new HttpError(400, "Create between 1 and 200 users at a time.");
  if (!/^[a-z0-9][a-z0-9._-]{0,20}$/.test(prefix)) throw new HttpError(400, "The prefix has up to 21 letters, digits, dots, dashes or underscores.");
  const limits = limitsFrom(b);
  const made = [];
  for (let i = 0; i < n; i++) {
    for (let tries = 0; tries < 5; tries++) {
      const name = `${prefix}${randomUsernameSuffix()}`;
      const password = randomPassword();
      try {
        await createUser(env, name, password, limits, now());
        made.push({ username: name, password });
        break;
      } catch (e) {
        if (!(e instanceof HttpError && e.status === 409)) throw e;
      }
    }
  }
  return json({ users: made }, 201);
}

async function adminUpdate(request, env, id) {
  await authAdmin(request, env);
  const user = await userById(env, id);
  if (!user) throw new HttpError(404, "No such user.");
  const b = await body(request);
  const limits = limitsFrom(b);
  const sets = [];
  const vals = [];
  const set = (col, v) => { vals.push(v); sets.push(`${col} = ?${vals.length}`); };
  for (const [k, v] of Object.entries(limits)) set(k, v);
  if (b.disabled !== undefined) set("disabled", b.disabled ? 1 : 0);
  if (b.password !== undefined) {
    if (!checkPassword(b.password)) throw new HttpError(400, "A password has at least 8 characters.");
    set("pass_hash", await hashPassword(String(b.password)));
    sets.push("token_version = token_version + 1");
  }
  if (b.reset_usage) {
    set("used_bytes", 0);
    set("used_seconds", 0);
    set("period_start", now());
  }
  if (b.sign_out) sets.push("token_version = token_version + 1");
  if (!sets.length) throw new HttpError(400, "Nothing to change.");
  vals.push(id);
  await env.DB.prepare(`UPDATE users SET ${sets.join(", ")} WHERE id = ?${vals.length}`).bind(...vals).run();
  if (b.sign_out || b.password !== undefined) await forgetDevices(env, id);
  const t = now();
  return json({ user: adminView(await userById(env, id), t, await busyDevices(env, { id }, "", t)) });
}

async function adminDelete(request, env, id) {
  await authAdmin(request, env);
  const r = await env.DB.prepare("DELETE FROM users WHERE id = ?1").bind(id).run();
  if (!r.meta.changes) throw new HttpError(404, "No such user.");
  await forgetDevices(env, id);
  return json({ ok: true });
}

/** The settings, and the code that pairs the team's Telegram chat while there is a bot and no chat yet. */
async function withPairing(env) {
  const out = await settings(env);
  const row = await env.DB.prepare("SELECT value FROM settings WHERE key = 'telegram_pair_code'").first();
  return { ...out, telegram_pair_code: out.telegram_bot_username && row ? row.value : "", stars_packages: salesCatalog(await salesConfig(env)) };
}

async function adminSettings(request, env) {
  await authAdmin(request, env);
  return json(await withPairing(env));
}

async function adminSaveSettings(request, env) {
  await authAdmin(request, env);
  const b = await body(request);
  const stmts = [];
  for (const k of Object.keys(DEFAULTS)) {
    if (b[k] === undefined) continue;
    let v = String(b[k]).slice(0, 500);
    if (k === "signup_open") v = b[k] === true || v === "1" ? "1" : "0";
    else if (k === "default_tier") {
      if (v !== "auto" && !Object.hasOwn(TIERS, v)) throw new HttpError(400, `The plan is auto or one of ${Object.keys(TIERS).join(", ")}.`);
    } else if (k === "plan_features") {
      // A JSON object of feature to plan; kept whole, as planFeatures reads it.
      let c;
      try {
        c = typeof b[k] === "string" ? JSON.parse(b[k] || "{}") : b[k];
      } catch {
        throw new HttpError(400, "plan_features must be a JSON object of feature to plan.");
      }
      if (!c || typeof c !== "object" || Array.isArray(c)) throw new HttpError(400, "plan_features must be a JSON object of feature to plan.");
      for (const [id, tier] of Object.entries(c)) {
        if (!Object.hasOwn(FEATURES, id)) throw new HttpError(400, `No feature is called ${id}.`);
        if (!Object.hasOwn(TIERS, tier)) throw new HttpError(400, `The plan for ${id} is one of ${Object.keys(TIERS).join(", ")}.`);
      }
      v = JSON.stringify(planFeatures(c));
    } else if (k.startsWith("download_")) {
      if (v && !/^https:\/\//.test(v)) throw new HttpError(400, "Download links must start with https://");
    } else if (k === "support_mail_status" || k === "telegram_bot_username" || k === "telegram_owner_chat" || k === "fx_rates" || k === "fx_at") {
      continue;
    } else if (k === "support_notify_to") {
      if (v && !/^[^@\s]{1,64}@[^@\s]{1,190}\.[^@\s]{2,}$/.test(v)) throw new HttpError(400, "That is not an email address.");
    } else if (k === "stars_size_discounts" || k === "stars_month_discounts") {
      // "GB:percent,..." and "months:percent,...", up to 90% each.
      const steps = parseSteps(v);
      if (!steps) throw new HttpError(400, `${k} is a list like 10:5,25:10 (${k === "stars_size_discounts" ? "GB a month" : "months"}:percent, at most 90%).`);
      v = steps.slice().reverse().map(([at, pct]) => `${at}:${pct}`).join(",");
    } else if (k === "stars_enabled") {
      v = b[k] === true || v === "1" ? "1" : "0";
    } else if (Object.hasOwn(SALES_RANGE, k)) {
      const [lo, hi] = SALES_RANGE[k];
      if (!/^\d{1,16}$/.test(v) || Number(v) < lo || Number(v) > hi) throw new HttpError(400, `${k} is a number from ${lo} to ${hi}.`);
    } else if (!/^\d{1,16}$/.test(v)) throw new HttpError(400, `Bad value for ${k}.`);
    stmts.push(env.DB.prepare("INSERT INTO settings (key, value) VALUES (?1, ?2) ON CONFLICT (key) DO UPDATE SET value = ?2").bind(k, v));
  }
  // Everything else is valid: now the Telegram bot. A token connects it (and says so if Telegram refuses it, before anything is saved), an empty one disconnects it.
  let warning = "";
  if (b.telegram_bot_token !== undefined) {
    const token = String(b.telegram_bot_token).trim();
    if (!token) await telegramDisconnect(env);
    else {
      const linked = await telegramConnect(env, request, token);
      if (linked.error) throw new HttpError(400, linked.error);
      warning = linked.warning || "";
    }
  }
  if (b.telegram_pair_reset === true) await telegramRepair(env);
  const checked = b.telegram_check === true ? await telegramCheck(env) : "";
  if (stmts.length) await env.DB.batch(stmts);
  plansCache = { at: 0, plans: null };
  return json({ ...(await withPairing(env)), ...(checked ? { telegram_check: checked } : {}), ...(warning ? { telegram_warning: warning } : {}) });
}

// ---------- Crowd learning ----------
//
// Apps whose users turned on "Share anonymous connection stats" report, a
// few times a day, how many tests of each kind of connection passed on their
// operator's network. A new install on that operator then starts with the
// order that works there. Reports carry no device or server details, and no
// address is stored: the rate limit keeps only a keyed hash of it, for a day.

const CROWD_KINDS = { proto: 16, style: 32, hello: 16, port: 8 };
// Diagnostics (migration 0012): at most this many named counters of each
// kind in a report, kept this many days.
const CROWD_NAMES = 64;
const CROWD_DIAG_DAYS = 30;
// Per-server reports: at most this many servers per report, kept this
// many days, and summed over the last few for the collector.
const CROWD_SERVERS = 300;
const CROWD_SERVER_DAYS = 7;
const CROWD_SERVER_WINDOW = 3;

/** Sums users' reports about servers over the last days into crowd.json for the collector (cmd/collect -crowd), with the protocols users chose and had no server for (demand), so it keeps more of what people ask for. */
export function crowdServersSummary(rows, today, demand = {}) {
  const servers = {};
  for (const r of rows) servers[r.key] = [r.ok, r.n];
  const out = { updated: new Date(today * DAY * 1000).toISOString(), days: CROWD_SERVER_WINDOW, servers };
  if (Object.keys(demand).length) out.demand = demand;
  return out;
}

async function publishCrowdServers(env) {
  if (!env.FILES) return null;
  const today = Math.floor(now() / DAY);
  const { results } = await env.DB.prepare(
    "SELECT key, SUM(ok) AS ok, SUM(n) AS n FROM crowd_servers WHERE day >= ?1 GROUP BY key HAVING SUM(n) >= 2",
  ).bind(today - CROWD_SERVER_WINDOW).all();
  // What people asked for and could not get, over the same days.
  const demand = {};
  try {
    const { results: asked } = await env.DB.prepare(
      "SELECT code, SUM(n) AS n FROM diag WHERE kind = 'demand' AND day >= ?1 GROUP BY code",
    ).bind(today - CROWD_SERVER_WINDOW).all();
    for (const r of asked) demand[r.code] = r.n;
  } catch {
    // The table arrives with migration 0012; until it is applied there is no demand.
  }
  const summary = crowdServersSummary(results, today, demand);
  await putList(env.FILES, "servers/crowd.json", new TextEncoder().encode(JSON.stringify(summary)).buffer);
  return summary;
}

/** The administrator (or a test) publishes the summary now instead of at the hour. */
async function adminPublishCrowd(request, env) {
  await authAdmin(request, env);
  const summary = await publishCrowdServers(env);
  if (!summary) throw new HttpError(503, "No file storage is bound.");
  return json({ servers: Object.keys(summary.servers).length });
}

/** The administrator reads the diagnostics: per version and platform, each named count over the last days (?days=7, at most 30), and the networks where connecting fails most, so what breaks where is plain before the next version. */
async function adminDiag(request, env, url) {
  await authAdmin(request, env);
  const days = Math.min(30, Math.max(1, Number(url.searchParams.get("days")) || 7));
  const since = Math.floor(now() / DAY) - days;
  const { results: counts } = await env.DB.prepare(
    `SELECT version, platform, kind, code, SUM(n) AS n, COUNT(DISTINCT asn) AS asns FROM diag
     WHERE day >= ?1 GROUP BY version, platform, kind, code ORDER BY version DESC, platform, kind, n DESC`,
  ).bind(since).all();
  const { results: failing } = await env.DB.prepare(
    `SELECT asn, code, SUM(n) AS n FROM diag
     WHERE kind = 'diag' AND day >= ?1 AND (code LIKE 'connect_fail%' OR code LIKE 'connect_no_server%')
     GROUP BY asn, code ORDER BY n DESC LIMIT 100`,
  ).bind(since).all();
  return json({ days, counts, failing_networks: failing });
}
const CROWD_DAYS = 14;
// Reports from one address in a day. The app reports only through the VPN,
// so a server's users all share its exit address (the project's own
// servers many of them): the cap must hold that crowd, and still keep one
// address from drowning the rest.
const CROWD_REPORTS_PER_ADDRESS = 60;
// An operator's numbers are published only once this many reports came in,
// so no one report can be told apart.
const CROWD_MIN_REPORTS = 3;

async function crowdReport(request, env) {
  const b = await body(request);
  const asn = b.asn;
  if (!Number.isInteger(asn) || asn < 1 || asn > 4294967295) throw new HttpError(400, "Bad request.");
  const rows = [];
  for (const [kind, size] of Object.entries(CROWD_KINDS)) {
    const list = b[kind] ?? [];
    if (!Array.isArray(list) || list.length > size) throw new HttpError(400, "Bad request.");
    for (const r of list) {
      if (!Array.isArray(r) || r.length !== 3 || !r.every(Number.isInteger)) throw new HttpError(400, "Bad request.");
      const [code, ok, n] = r;
      if (code < 0 || code >= size || n < 1 || ok < 0 || ok > n) throw new HttpError(400, "Bad request.");
      // One report counts for at most 500 tests of a kind.
      const scale = Math.min(1, 500 / n);
      rows.push([kind, code, Math.round(ok * scale), Math.round(n * scale)]);
    }
  }
  // Per server: [key, ok, n], the key a 64-bit hash in hex (no address).
  const servers = b.servers ?? [];
  if (!Array.isArray(servers) || servers.length > CROWD_SERVERS) throw new HttpError(400, "Bad request.");
  const srows = [];
  for (const r of servers) {
    if (!Array.isArray(r) || r.length !== 3 || typeof r[0] !== "string" || !/^[0-9a-f]{1,16}$/.test(r[0])) throw new HttpError(400, "Bad request.");
    const [key, ok, n] = r;
    if (!Number.isInteger(ok) || !Number.isInteger(n) || n < 1 || ok < 0 || ok > n) throw new HttpError(400, "Bad request.");
    // One report counts for at most 20 tests of a server.
    const scale = Math.min(1, 20 / n);
    srows.push([key, Math.round(ok * scale), Math.round(n * scale)]);
  }
  const limit = "stats:" + (await clientTag(request, env));
  if (await blocked(env, limit, CROWD_REPORTS_PER_ADDRESS, DAY)) throw new HttpError(429, "Too many reports.");
  await count(env, limit, DAY);
  const day = Math.floor(now() / DAY);
  const stmts = rows.map(([kind, code, ok, n]) =>
    env.DB.prepare(
      `INSERT INTO crowd (asn, kind, code, day, ok, n) VALUES (?1, ?2, ?3, ?4, ?5, ?6)
       ON CONFLICT (asn, kind, code, day) DO UPDATE SET ok = ok + ?5, n = n + ?6`,
    ).bind(asn, kind, code, day, ok, n));
  for (const [key, ok, n] of srows) {
    stmts.push(env.DB.prepare(
      `INSERT INTO crowd_servers (key, day, ok, n) VALUES (?1, ?2, ?3, ?4)
       ON CONFLICT (key, day) DO UPDATE SET ok = ok + ?3, n = n + ?4`,
    ).bind(key, day, ok, n));
  }
  stmts.push(env.DB.prepare("DELETE FROM crowd_servers WHERE day < ?1").bind(day - CROWD_SERVER_DAYS));
  stmts.push(
    env.DB.prepare(
      `INSERT INTO crowd_reports (asn, day, reports) VALUES (?1, ?2, 1)
       ON CONFLICT (asn, day) DO UPDATE SET reports = reports + 1`,
    ).bind(asn, day),
    env.DB.prepare("DELETE FROM crowd WHERE day < ?1").bind(day - CROWD_DAYS),
    env.DB.prepare("DELETE FROM crowd_reports WHERE day < ?1").bind(day - CROWD_DAYS),
  );
  // Diagnostics (0012_diag.sql): the app's version and platform, named
  // counts of what happened (diag) and of the protocols chosen that had no
  // server (demand). Names and counts only; one report counts for at most
  // 1000 of a name. A report without a version carries none.
  const version = typeof b.version === "string" && /^[0-9][0-9A-Za-z.+-]{0,23}$/.test(b.version) ? b.version : "";
  const platform = ["android", "windows", "darwin", "linux"].includes(b.platform) ? b.platform : "other";
  if (version) {
    for (const kind of ["demand", "diag"]) {
      const m = b[kind];
      if (m == null) continue;
      if (typeof m !== "object" || Array.isArray(m) || Object.keys(m).length > CROWD_NAMES) throw new HttpError(400, "Bad request.");
      for (const [code, n] of Object.entries(m)) {
        if (!/^[a-z0-9_.-]{1,40}$/.test(code) || !Number.isInteger(n) || n < 1) throw new HttpError(400, "Bad request.");
        stmts.push(env.DB.prepare(
          `INSERT INTO diag (day, version, platform, asn, kind, code, n) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)
           ON CONFLICT (day, version, platform, asn, kind, code) DO UPDATE SET n = n + ?7`,
        ).bind(day, version, platform, asn, kind, code, Math.min(n, 1000)));
      }
    }
    stmts.push(env.DB.prepare("DELETE FROM diag WHERE day < ?1").bind(day - CROWD_DIAG_DAYS));
  }
  await env.DB.batch(stmts);
  return json({ ok: true });
}

// GET stats lists what passes on each network (by ASN) that enough
// installs reported. With ?asn=self it gives only the asker's own network,
// and says which one that is (an app cannot tell its ASN by itself).
async function crowdSummary(request, env, url) {
  const self = request.cf?.asn ?? 0;
  const only = url.searchParams.get("asn") === "self" ? self : 0;
  const since = Math.floor(now() / DAY) - CROWD_DAYS;
  const { results: enough } = await env.DB.prepare(
    "SELECT asn FROM crowd_reports WHERE day >= ?1 GROUP BY asn HAVING SUM(reports) >= ?2",
  ).bind(since, CROWD_MIN_REPORTS).all();
  const keep = new Set(enough.map((r) => r.asn));
  const { results } = await env.DB.prepare(
    "SELECT asn, kind, code, SUM(ok) AS ok, SUM(n) AS n FROM crowd WHERE day >= ?1 GROUP BY asn, kind, code",
  ).bind(since).all();
  const out = {};
  for (const r of results) {
    if (!keep.has(r.asn) || (only && r.asn !== only)) continue;
    const a = (out[r.asn] ??= {});
    (a[r.kind] ??= []).push([r.code, r.ok, r.n]);
  }
  return json({ asn: self, asns: out });
}

// download serves the release files (the APK and the Windows installer)
// from GitHub's releases through Cloudflare, for networks where GitHub's
// downloads break off or are filtered: /dl/v0.4.1/Albatross-0.4.1-android.apk.
// Only those files; ranges pass through so interrupted downloads resume,
// and the edge keeps a copy for a day.
const DOWNLOADS = /^\/dl\/(v\d+\.\d+\.\d+)\/(Albatross-\d+\.\d+\.\d+-android\.apk|Albatross-Setup-\d+\.\d+\.\d+\.exe)$/;
async function download(request, url, env) {
  const m = DOWNLOADS.exec(url.pathname);
  if (!m) return new Response("Not found", { status: 404 });
  if (env.FILES) {
    const r = await serveFile(request, env.FILES, m[2]);
    if (r) return r;
  }
  const headers = {};
  const range = request.headers.get("Range");
  if (range) headers.Range = range;
  // The public copy of the release (alba-data): this repository is private.
  const up = await fetch(`https://github.com/Albatrossvpn/alba-data/releases/download/${m[1]}/${m[2]}`, {
    headers,
    redirect: "follow",
    cf: { cacheTtl: 86400, cacheEverything: true },
  });
  if (!up.ok && up.status !== 206) return new Response("Not found", { status: 404 });
  const out = new Headers();
  for (const h of ["Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"]) {
    const v = up.headers.get(h);
    if (v) out.set(h, v);
  }
  out.set("Content-Type", m[2].endsWith(".apk") ? "application/vnd.android.package-archive" : "application/octet-stream");
  out.set("Content-Disposition", `attachment; filename="${m[2]}"`);
  out.set("Cache-Control", "public, max-age=86400");
  out.set("X-Content-Type-Options", "nosniff");
  return new Response(up.body, { status: up.status, headers: out });
}

// mirror serves the app's own files (lists, update manifest, ad list) from
// the project's own storage, which CI fills (deploy/files/publish-lists.sh),
// or from GitHub while a file is not there yet and the repository is public.
// Only those files.
async function mirror(request, url, env) {
  const path = url.pathname.slice("/list/".length);
  if (!LIST_PATH.test(path)) return new Response("Not found", { status: 404 });
  if (env.FILES) {
    const r = await serveList(request, env.FILES, path);
    if (r) return r;
  }
  const up = await fetch("https://raw.githubusercontent.com/Albatrossvpn/Albatross/" + path, { cf: { cacheTtl: 600, cacheEverything: true } });
  if (!up.ok) return new Response("Not found", { status: 404 });
  return new Response(up.body, {
    status: 200,
    headers: { "Content-Type": "text/plain; charset=utf-8", "Cache-Control": "public, max-age=600" },
  });
}

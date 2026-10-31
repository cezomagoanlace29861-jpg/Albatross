import { $, ago, api, esc, fmtBytes, fmtDate, fmtTime, frame, planBadge, toast } from "/site.js";

const GB = 1024 ** 3;
const KEY = "admin-session";
let token = "";
try { token = sessionStorage.getItem(KEY) || ""; } catch {}
let offset = 0;
const LIMIT = 25;
let users = [];
let editing = null;

const call = (method, path, body) => api(method, "admin/" + path, { token, body });

function signedOut() {
  token = "";
  try { sessionStorage.removeItem(KEY); } catch {}
  $("#auth").classList.remove("hidden");
  $("#panel").classList.add("hidden");
  $("#logout").classList.add("hidden");
}

/** Runs an administrator call; a refused session goes back to the sign-in. */
async function guard(fn, errEl) {
  try {
    return await fn();
  } catch (e) {
    if (e.status === 401) signedOut();
    else if (errEl) errEl.textContent = e.message;
    else toast(e.message);
  }
}

// ---------- Limits fields ----------

const toDate = (unix) => (unix ? new Date(unix * 1000).toISOString().slice(0, 10) : "");
const fromDate = (s) => (s ? Math.floor(new Date(s + "T23:59:59Z").getTime() / 1000) : 0);
const num = (v) => (v === "" || v == null ? 0 : Number(v));

function limitsFrom(p) {
  return {
    tier: $(`#${p}-tier`).value,
    quota_bytes: Math.round(num($(`#${p}-quota`).value) * GB),
    max_seconds: Math.round(num($(`#${p}-hours`).value) * 3600),
    reset_days: Math.round(num($(`#${p}-reset`).value)),
    expires_at: fromDate($(`#${p}-expires`).value),
    note: $(`#${p}-note`).value.trim(),
  };
}

function fillLimits(p, u) {
  $(`#${p}-tier`).value = u.tier || "auto";
  $(`#${p}-quota`).value = u.quota_bytes ? +(u.quota_bytes / GB).toFixed(3) : "";
  $(`#${p}-hours`).value = u.max_seconds ? +(u.max_seconds / 3600).toFixed(2) : "";
  $(`#${p}-reset`).value = u.reset_days || "";
  $(`#${p}-expires`).value = toDate(u.expires_at);
  $(`#${p}-note`).value = u.note || "";
}

// ---------- Overview and list ----------

async function loadStats() {
  const s = await guard(() => call("GET", "stats"));
  if (!s) return;
  const cards = [
    [s.users, "Users"], [s.active_day, "Active in 24 h"], [s.active_month, "Active in 30 days"],
    [s.disabled, "Turned off"], [fmtBytes(s.total_bytes), "Total data"], [fmtTime(s.total_seconds), "Total time"],
  ];
  // The servers accounts get: those proven healthy from inside Iran. None
  // means every account is left with WARP, Psiphon, Tor and the edge.
  if (s.servers) {
    const by = s.servers.by && s.servers.by !== "none" ? `proven by ${s.servers.by.replace("+", " and ")}` : "nothing proves any now";
    cards.push([s.servers.proven, `Servers given to accounts (${by})`]);
  }
  $("#stats").innerHTML = cards.map(([v, k]) => `<div class="stat"><b>${esc(v)}</b><span>${esc(k)}</span></div>`).join("");
}

function usageCell(used, limit, fmt) {
  if (!limit) return `${esc(fmt(used))}<div class="muted small">no limit</div>`;
  const frac = Math.min(1, used / limit);
  const cls = frac >= 1 ? "full" : frac >= 0.85 ? "warn" : "";
  return `${esc(fmt(Math.min(used, limit)))} <span class="muted">/ ${esc(fmt(limit))}</span><div class="bar mini"><i class="${cls}" data-w="${(frac * 100).toFixed(0)}"></i></div>`;
}

const statusPill = (u) =>
  u.status === "active" ? `<span class="pill">Active</span>`
    : `<span class="pill ${u.status === "disabled" ? "bad" : "warn"}">${esc({ disabled: "Off", expired: "Expired", quota: "Data used", time: "Time used", devices: "Devices full" }[u.status] || u.status)}</span>`;

let tiers = {};
let features = {}; // what the plans may include (the service's FEATURES)
const planName = (tier) => (tiers[tier] ? tiers[tier].name : tier);
const planCell = (u) => `${planBadge(u.tier, planName(u.tier))}<div class="muted small">${u.max_devices ? `${u.devices} of ${u.max_devices}` : u.devices} ${u.devices === 1 && !u.max_devices ? "device" : "devices"}</div>`;

/** Fills every plan selector from the service's list of plans. */
function fillPlans() {
  const opts = Object.entries(tiers).map(([k, t]) => `<option value="${esc(k)}">${esc(t.name)} · ${t.devices ? `${t.devices} ${t.devices === 1 ? "device" : "devices"}` : "any number of devices"}</option>`).join("");
  // "Auto" lets the data allowance pick the plan: no limit VIP, 100 GB
  // and up Gold, 10 GB and up Silver, less Bronze.
  const auto = `<option value="auto">Auto · by data allowance</option>`;
  for (const id of ["u-tier", "b-tier", "t-tier"]) $("#" + id).innerHTML = auto + opts;
}

async function loadUsers() {
  const q = $("#q").value.trim();
  const r = await guard(() => call("GET", `users?q=${encodeURIComponent(q)}&limit=${LIMIT}&offset=${offset}`));
  if (!r) return;
  users = r.users;
  $("#rows").innerHTML = users.map((u) => `<tr>
    <td><b>${esc(u.username)}</b>${u.note ? `<div class="muted small">${esc(u.note)}</div>` : ""}</td>
    <td>${statusPill(u)}</td>
    <td>${planCell(u)}</td>
    <td>${usageCell(u.used_bytes, u.quota_bytes, fmtBytes)}</td>
    <td>${usageCell(u.used_seconds, u.max_seconds, fmtTime)}</td>
    <td>${u.expires_at ? esc(fmtDate(u.expires_at)) : "—"}</td>
    <td>${esc(ago(u.last_seen))}</td>
    <td><button class="btn small" data-edit="${u.id}">Edit</button></td></tr>`).join("") ||
    `<tr><td colspan="8" class="muted">No users.</td></tr>`;
  for (const i of document.querySelectorAll("#rows .bar i")) i.style.width = i.dataset.w + "%";
  const from = r.total ? offset + 1 : 0;
  $("#count").textContent = `${from}–${offset + users.length} of ${r.total}`;
  $("#prev").disabled = offset === 0;
  $("#next").disabled = offset + LIMIT >= r.total;
}

const refresh = () => Promise.all([loadStats(), loadUsers()]);

function enter(t) {
  token = t;
  try { sessionStorage.setItem(KEY, t); } catch {}
  $("#auth").classList.add("hidden");
  $("#panel").classList.remove("hidden");
  $("#logout").classList.remove("hidden");
  refresh();
}

// ---------- Sign in ----------

$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("#a-error").textContent = "";
  try {
    const r = await api("POST", "admin/login", { body: { username: $("#a-user").value, password: $("#a-pass").value } });
    $("#a-pass").value = "";
    enter(r.token);
  } catch (err) {
    $("#a-error").textContent = err.message;
  }
});
$("#logout").addEventListener("click", signedOut);

// ---------- A password to hand over ----------

function showSecret(title, secret) {
  $("#x-title").textContent = title;
  $("#x-secret").textContent = secret;
  $("#secret-dialog").showModal();
}
$("#x-close").addEventListener("click", () => $("#secret-dialog").close());
$("#x-copy").addEventListener("click", () => copy($("#x-secret").textContent));

async function copy(text) {
  try { await navigator.clipboard.writeText(text); toast("Copied."); } catch { toast("Select the text and copy it."); }
}

// ---------- Create or edit a user ----------

function openUser(u) {
  editing = u;
  $("#u-error").textContent = "";
  $("#u-title").innerHTML = u ? `Edit ${esc(u.username)} ${planBadge(u.tier, planName(u.tier))}` : "New user";
  $("#u-name-field").classList.toggle("hidden", !!u);
  $("#u-pass-field").classList.toggle("hidden", !!u);
  $("#u-extra").classList.toggle("hidden", !u);
  $("#u-save").textContent = u ? "Save" : "Create";
  $("#u-name").value = "";
  $("#u-pass").value = "";
  fillLimits("u", u || { reset_days: 0, quota_bytes: 5 * GB, tier: "auto" });
  if (u) $("#u-toggle").textContent = u.disabled ? "Turn on" : "Turn off";
  $("#user-dialog").showModal();
}

$("#new").addEventListener("click", () => openUser(null));
$("#rows").addEventListener("click", (e) => {
  const id = e.target.dataset && e.target.dataset.edit;
  if (id) openUser(users.find((u) => u.id === Number(id)));
});
$("#u-cancel").addEventListener("click", () => $("#user-dialog").close());

$("#user-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const err = $("#u-error");
  err.textContent = "";
  if (editing) {
    const r = await guard(() => call("PATCH", `users/${editing.id}`, limitsFrom("u")), err);
    if (r) { $("#user-dialog").close(); toast("Saved."); refresh(); }
  } else {
    const r = await guard(() => call("POST", "users", { username: $("#u-name").value, password: $("#u-pass").value || undefined, ...limitsFrom("u") }), err);
    if (r) {
      $("#user-dialog").close();
      showSecret(`Password for ${r.user.username}`, r.password);
      refresh();
    }
  }
});

async function patch(body, done) {
  const r = await guard(() => call("PATCH", `users/${editing.id}`, body), $("#u-error"));
  if (r) { editing = r.user; refresh(); done && done(r); }
}
$("#u-reset-usage").addEventListener("click", () => patch({ reset_usage: true }, () => toast("Recharged: the allowance is full again.")));
$("#u-signout").addEventListener("click", () => patch({ sign_out: true }, () => toast("Signed out everywhere.")));
$("#u-toggle").addEventListener("click", () => patch({ disabled: !editing.disabled }, (r) => {
  $("#u-toggle").textContent = r.user.disabled ? "Turn on" : "Turn off";
  toast(r.user.disabled ? "Turned off." : "Turned on.");
}));
$("#u-reset-pass").addEventListener("click", () => {
  const pw = Array.from(crypto.getRandomValues(new Uint8Array(14)), (b) => "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"[b % 54]).join("");
  const name = editing.username;
  patch({ password: pw }, () => { $("#user-dialog").close(); showSecret(`New password for ${name}`, pw); });
});
$("#u-delete").addEventListener("click", async () => {
  if (!confirm(`Delete ${editing.username}? This cannot be undone.`)) return;
  const r = await guard(() => call("DELETE", `users/${editing.id}`), $("#u-error"));
  if (r) { $("#user-dialog").close(); toast("Deleted."); refresh(); }
});

// ---------- Create many ----------

let bulkRows = [];
$("#bulk").addEventListener("click", () => {
  $("#b-fields").classList.remove("hidden");
  $("#b-result").classList.add("hidden");
  for (const id of ["b-copy", "b-download"]) $("#" + id).classList.add("hidden");
  $("#b-create").classList.remove("hidden");
  $("#b-error").textContent = "";
  fillLimits("b", { reset_days: 0, quota_bytes: 5 * GB, tier: "auto" });
  $("#bulk-dialog").showModal();
});
$("#b-cancel").addEventListener("click", () => $("#bulk-dialog").close());
$("#bulk-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const err = $("#b-error");
  err.textContent = "";
  const r = await guard(() => call("POST", "users/bulk", { prefix: $("#b-prefix").value, count: Number($("#b-count").value), ...limitsFrom("b") }), err);
  if (!r) return;
  bulkRows = r.users;
  $("#b-out").value = "username,password\n" + bulkRows.map((u) => `${u.username},${u.password}`).join("\n");
  $("#b-fields").classList.add("hidden");
  $("#b-result").classList.remove("hidden");
  $("#b-create").classList.add("hidden");
  for (const id of ["b-copy", "b-download"]) $("#" + id).classList.remove("hidden");
  refresh();
});
$("#b-copy").addEventListener("click", () => copy($("#b-out").value));
$("#b-download").addEventListener("click", () => {
  const a = document.createElement("a");
  a.href = URL.createObjectURL(new Blob([$("#b-out").value + "\n"], { type: "text/csv" }));
  a.download = "accounts.csv";
  a.click();
  URL.revokeObjectURL(a.href);
});

// ---------- Settings ----------

$("#settings").addEventListener("click", async () => {
  const s = await guard(() => call("GET", "settings"));
  if (!s) return;
  $("#t-signup").checked = s.signup_open === "1";
  $("#t-quota").value = +(Number(s.default_quota_bytes) / GB).toFixed(3);
  $("#t-hours").value = +(Number(s.default_max_seconds) / 3600).toFixed(2);
  $("#t-reset").value = s.default_reset_days;
  $("#t-valid").value = s.default_valid_days;
  $("#t-tier").value = s.default_tier || "auto";
  $("#t-win").value = s.download_windows;
  $("#t-and").value = s.download_android;
  $("#t-support").value = s.support_notify_to || "";
  $("#t-support-status").textContent = s.support_mail_status ? `Last summary: ${s.support_mail_status}.` : "";
  showTelegram(s);
  showStars(s);
  // Which plan brings each feature: the saved choice over the features' own.
  let chosen = {};
  try { chosen = JSON.parse(s.plan_features || "{}"); } catch {}
  $("#t-features").innerHTML = Object.entries(features).map(([id, f]) => `<label class="field"><span>${esc(f.name)}</span><select data-feature="${esc(id)}">${
    Object.entries(tiers).map(([k, t]) => `<option value="${esc(k)}"${(chosen[id] || f.tier) === k ? " selected" : ""}>${esc(t.name)} and above</option>`).join("")}</select></label>`).join("");
  $("#t-error").textContent = "";
  $("#settings-dialog").showModal();
});

// ---------- The Telegram bot ----------

// What the bot is doing, and what is left to set up: the token, then pairing the team's chat.
function showTelegram(s) {
  const bot = s.telegram_bot_username;
  $("#t-tg").value = "";
  $("#t-tg-actions").classList.toggle("hidden", !bot);
  const status = $("#t-tg-status");
  status.textContent = "";
  if (!bot) { status.textContent = "Not connected."; return; }
  status.append(`Connected as @${bot}. `);
  if (s.telegram_owner_chat) status.append("Tickets arrive in your Telegram chat, and you answer by replying to them. ");
  // A code, whenever there is one: to pair the first chat, or another one in its place.
  if (s.telegram_pair_code) {
    status.append(s.telegram_owner_chat ? "To move the tickets to another chat, send " : "To receive tickets in Telegram, open the bot from the account or group that should get them and send ");
    const code = document.createElement("code");
    code.textContent = `/pair ${s.telegram_pair_code}`;
    status.append(code, " there. The code works once.");
  }
}
// Selling data with Stars: the settings, and the packages they make.
function showStars(s) {
  $("#t-stars").checked = s.stars_enabled === "1";
  $("#t-stars-price").value = s.stars_price_toman;
  $("#t-stars-usd").value = s.stars_usd_toman;
  $("#t-stars-worth").value = s.stars_usd_per_1000;
  $("#t-stars-buy").value = s.stars_buy_usd_per_1000;
  $("#t-stars-days").value = s.stars_valid_days;
  $("#t-stars-size").value = s.stars_size_discounts ?? "";
  $("#t-stars-months").value = s.stars_month_discounts ?? "";
  const list = (s.stars_packages || []).map((p) => `${p.gb} GB = ${p.stars} ⭐ (${p.plan}${p.save ? `, ${p.save}% off` : ""})`).join(", ");
  $("#t-stars-preview").textContent = list ? `Packages at these prices: ${list}. Saved settings make these; the bot shows them once they are saved.` : "";
}
$("#t-tg-off").addEventListener("click", async () => {
  if (!confirm("Disconnect the Telegram bot? Open tickets and chats are forgotten.")) return;
  const s = await guard(() => call("PUT", "settings", { telegram_bot_token: "" }), $("#t-error"));
  if (s) { showTelegram(s); toast("The bot is disconnected."); }
});
$("#t-tg-check").addEventListener("click", async () => {
  const s = await guard(() => call("PUT", "settings", { telegram_check: true }), $("#t-error"));
  if (!s) return;
  showTelegram(s);
  $("#t-tg-status").append(` ${s.telegram_check || ""}`);
});
$("#t-tg-pair").addEventListener("click", async () => {
  const s = await guard(() => call("PUT", "settings", { telegram_pair_reset: true }), $("#t-error"));
  if (s) { showTelegram(s); toast("A new pairing code is ready."); }
});
$("#t-cancel").addEventListener("click", () => $("#settings-dialog").close());
$("#settings-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const connecting = !!$("#t-tg").value.trim();
  const r = await guard(() => call("PUT", "settings", {
    signup_open: $("#t-signup").checked,
    default_quota_bytes: String(Math.round(num($("#t-quota").value) * GB)),
    default_max_seconds: String(Math.round(num($("#t-hours").value) * 3600)),
    default_reset_days: String(Math.round(num($("#t-reset").value))),
    default_valid_days: String(Math.round(num($("#t-valid").value))),
    default_tier: $("#t-tier").value,
    download_windows: $("#t-win").value.trim(),
    download_android: $("#t-and").value.trim(),
    support_notify_to: $("#t-support").value.trim(),
    stars_enabled: $("#t-stars").checked,
    stars_price_toman: String(Math.round(num($("#t-stars-price").value))),
    stars_usd_toman: String(Math.round(num($("#t-stars-usd").value))),
    stars_usd_per_1000: String(Math.round(num($("#t-stars-worth").value))),
    stars_buy_usd_per_1000: String(Math.round(num($("#t-stars-buy").value))),
    stars_valid_days: String(Math.round(num($("#t-stars-days").value))),
    stars_size_discounts: $("#t-stars-size").value.trim(),
    stars_month_discounts: $("#t-stars-months").value.trim(),
    // A token connects the bot (and Telegram's refusal shows here); left empty, the bot stays as it is.
    ...($("#t-tg").value.trim() ? { telegram_bot_token: $("#t-tg").value.trim() } : {}),
    plan_features: Object.fromEntries([...document.querySelectorAll("#t-features select")].map((el) => [el.dataset.feature, el.value])),
  }), $("#t-error"));
  if (!r) return;
  // A bot that was just connected stays on screen: its pairing code is the next step.
  if (connecting) {
    showTelegram(r);
    showStars(r);
    // Telegram may refuse the bot's menu or texts: the bot works, and the panel says what to do.
    if (r.telegram_warning) $("#t-tg-status").append(` ${r.telegram_warning}`);
    toast("Saved. The bot is connected.");
    return;
  }
  $("#settings-dialog").close();
  toast("Saved.");
});

// ---------- List controls ----------

let timer;
$("#q").addEventListener("input", () => { clearTimeout(timer); timer = setTimeout(() => { offset = 0; loadUsers(); }, 250); });
$("#prev").addEventListener("click", () => { offset = Math.max(0, offset - LIMIT); loadUsers(); });
$("#next").addEventListener("click", () => { offset += LIMIT; loadUsers(); });

const shown = await frame();
tiers = shown.tiers || {};
features = shown.features || {};
fillPlans();
if (token) enter(token);

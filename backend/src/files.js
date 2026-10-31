// The project's own storage (an R2 bucket): the release files (the Android
// APK and the Windows installer) and the app's own files (lists, update
// manifest, ad list), served by the website and the account service, so
// nothing the app or a visitor downloads depends on GitHub. Shared by the
// website Worker (site/src/index.js) and the account service.

/** The files that may be stored and served, by name. */
export const FILE_NAME = /^(Albatross-\d+\.\d+\.\d+-android\.apk|Albatross-Setup-\d+\.\d+\.\d+\.exe)$/;

/** Where the files come from: this project's GitHub releases, and only those. */
export const FILE_SOURCE = /^https:\/\/github\.com\/Albatrossvpn\/Albatross\/releases\/download\/v\d+\.\d+\.\d+\/(Albatross-[^/?#]+)$/;

export function contentType(name) {
  return name.endsWith(".apk") ? "application/vnd.android.package-archive" : "application/octet-stream";
}

/** "bytes=a-b", "bytes=a-" or "bytes=-n" as an R2 range, or null for the whole file. */
export function parseRange(h, size) {
  const m = /^bytes=(\d*)-(\d*)$/.exec(h || "");
  if (!m || (!m[1] && !m[2])) return null;
  if (!m[1]) {
    const n = Math.min(Number(m[2]), size);
    return n > 0 ? { offset: size - n, length: n } : null;
  }
  const start = Number(m[1]);
  if (start >= size) return { invalid: true };
  const end = m[2] ? Math.min(Number(m[2]), size - 1) : size - 1;
  if (end < start) return { invalid: true };
  return { offset: start, length: end - start + 1 };
}

/**
 * The app's own files, by their path in the branch they were first
 * published to: the collector's lists, the update manifest and the ad list
 * (with its source form and licences, which its GPL licence asks to be
 * offered from the same place).
 * They live under list/ in the bucket and are served at /list/<path>.
 */
export const LIST_PATH = /^(servers\/((hot\.txt|new\.txt|fresh\.txt|proven\.txt|all-[1-8]\.txt)(\.sealed)?|tor\.json|meta\.json|reach\.json|crowd\.json|iran\.json|sources\.txt|channels\.txt|discover\.json|own-[0-4]\.bin(\.sealed)?)|main\/update\.json|rules\/(ads\.srs|ads\.json|README\.md|LICENSE-GPL-3\.0\.txt|LICENSE-AGPL-3\.0\.txt)|tools\/(collect-linux-(amd64|arm64)|tester-install\.sh|(sing-box|xray)-linux-(amd64|arm64)))$/;

// Where the collector keeps the lists' keys (not under list/, so never
// served as a file): see listKeys in index.js.
export const LIST_KEYS = "private/listkeys.json";

/** The most a published list may weigh (the largest is about 6 MB). */
export const MAX_LIST = 32 * 1024 * 1024;

export function listType(path) {
  if (path.endsWith(".json")) return "application/json; charset=utf-8";
  if (path.endsWith(".srs") || path.endsWith(".bin") || path.endsWith(".sealed") || path.startsWith("tools/")) return "application/octet-stream";
  return "text/plain; charset=utf-8";
}

/**
 * Serves one stored object for a GET or HEAD request, with ranges (so a
 * cut download resumes) and conditional requests. Null when it is not
 * stored, so the caller may fall back or answer 404.
 */
async function serveObject(request, bucket, key, base) {
  const head = await bucket.head(key);
  if (!head) return null;
  const range = parseRange(request.headers.get("Range"), head.size);
  if (range && range.invalid) {
    return new Response("Range not satisfiable", { status: 416, headers: { "Content-Range": `bytes */${head.size}` } });
  }
  const headers = new Headers({ ...base, "Accept-Ranges": "bytes", "X-Content-Type-Options": "nosniff", "ETag": head.httpEtag });
  const inm = request.headers.get("If-None-Match");
  if (inm && !range && inm.split(",").map((s) => s.trim()).includes(head.httpEtag)) {
    return new Response(null, { status: 304, headers });
  }
  if (range) {
    headers.set("Content-Range", `bytes ${range.offset}-${range.offset + range.length - 1}/${head.size}`);
    headers.set("Content-Length", String(range.length));
  } else {
    headers.set("Content-Length", String(head.size));
  }
  if (request.method === "HEAD") return new Response(null, { status: range ? 206 : 200, headers });
  const obj = await bucket.get(key, range ? { range } : undefined);
  if (!obj) return null;
  return new Response(obj.body, { status: range ? 206 : 200, headers });
}

/** A release file (FILE_NAME), or null when it is not stored. */
export async function serveFile(request, bucket, name) {
  if (!FILE_NAME.test(name)) return null;
  return serveObject(request, bucket, name, {
    "Content-Type": contentType(name),
    "Content-Disposition": `attachment; filename="${name}"`,
    "Cache-Control": "public, max-age=86400",
  });
}

/** One of the app's own files (LIST_PATH), or null when it is not stored. */
export async function serveList(request, bucket, path) {
  if (!LIST_PATH.test(path)) return null;
  return serveObject(request, bucket, "list/" + path, { "Content-Type": listType(path), "Cache-Control": "public, max-age=300" });
}

/** Stores one of the app's own files; throws with a message for the caller. */
export async function putList(bucket, path, body) {
  if (!LIST_PATH.test(path)) throw new Error("Not one of the app's files.");
  if (!body || body.byteLength === 0) throw new Error("Empty file.");
  if (body.byteLength > MAX_LIST) throw new Error("File too large.");
  await bucket.put("list/" + path, body, { httpMetadata: { contentType: listType(path) } });
  return { path, size: body.byteLength };
}

/**
 * Stores a release file from its source, streamed straight into the
 * bucket. Returns what was stored; throws with a message for the caller
 * on a bad name or source, or a failed fetch.
 */
export async function pullFile(bucket, name, source, { force = false } = {}) {
  if (!FILE_NAME.test(name)) throw new Error("Not a release file name.");
  const m = FILE_SOURCE.exec(source || "");
  if (!m || m[1] !== name) throw new Error("Not a release address for that file.");
  const have = await bucket.head(name);
  const up = await fetch(source, { redirect: "follow", headers: { "Accept-Encoding": "identity" } });
  if (!up.ok) throw new Error(`The source answered ${up.status}.`);
  const size = Number(up.headers.get("Content-Length"));
  if (!size) throw new Error("The source gives no length.");
  if (have && have.size === size && !force) {
    await up.body.cancel();
    return { name, size, stored: false };
  }
  const { readable, writable } = new FixedLengthStream(size);
  const pump = up.body.pipeTo(writable);
  await bucket.put(name, readable, { httpMetadata: { contentType: contentType(name) }, customMetadata: { source } });
  await pump;
  return { name, size, stored: true };
}

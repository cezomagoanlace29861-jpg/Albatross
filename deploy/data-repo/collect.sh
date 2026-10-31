#!/usr/bin/env bash
# One collection run in the public repository alba-data (its workflow
# checks this repository out into src/ and runs this from there). Its
# minutes are free, so collections run back to back; this repository stays
# private and nothing of it is kept there.
#
# What the run learns stays out of the public log and the public branch:
# the collector's output goes to a file that is not kept, the log shows
# counts, and the servers branch there gets only the sealed lists (AES-256-
# GCM with a key that changes every run and that only signed-in users get
# from the account service; the own servers sealed with the build's key
# too). The statistics, the sources, the keys and every plain file go to
# the project's own storage only.
#
# Environment: CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID, ADMIN_USER,
# ADMIN_PASSWORD, OWN_SERVERS, OWN_RELAYS, OWN_KEY, GLOBALPING_TOKEN,
# GLOBALPING_TAGS (as for .github/workflows/collect.yml), PUBLISH_REPO and
# PUBLISH_TOKEN (the public repository and a token that may push to it).
set -uo pipefail
src="$(cd "$(dirname "$0")/../.." && pwd)"
tmp="${RUNNER_TEMP:-$(mktemp -d)}"
cd "$src"
. deploy/files/lib.sh
(cd backend && npm ci --silent >/dev/null 2>&1) || { echo "::error::npm ci failed"; exit 1; }
have_wrangler || { echo "::error::the Cloudflare credentials are missing"; exit 1; }

get() { (cd backend && npx wrangler r2 object get "$(bucket_name)/$1" --file "$2" --remote >/dev/null 2>&1) || { rm -f "$2"; return 1; }; }

# The last run's state, from the project's own storage.
mkdir -p "$tmp/prev"
for f in hot.txt.sealed fresh.txt.sealed new.txt.sealed iran-tcp.json; do get "list/servers/$f" "$tmp/prev/$f"; done
get private/source-stats.json "$tmp/prev/source-stats.json"
get list/servers/crowd.json "$tmp/crowd.json" || true
get list/servers/iran.json "$tmp/iran.json" || true
for f in sources.txt channels.txt; do get "list/servers/$f" "$tmp/$f"; done
for _ in 1 2 3; do get private/listkeys.json "$tmp/listkeys.json" && break; sleep 10; done
[ -s "$tmp/listkeys.json" ] || echo "no list keys yet; starting the first"
for f in own-servers.txt own-relays.txt; do get "private/$f" "$tmp/$f"; done

# The cores the real test connects through.
SINGBOX_VERSION="1.14.4" XRAY_VERSION="26.10.21"
mkdir -p "$tmp/cores"
(cd "$tmp/cores" &&
  curl -fsSL --retry 4 -o sb.tgz "https://github.com/SagerNet/sing-box/releases/download/v${SINGBOX_VERSION}/sing-box-${SINGBOX_VERSION}-linux-amd64.tar.gz" &&
  tar xzf sb.tgz --strip-components=1 &&
  curl -fsSL --retry 4 -o x.zip "https://github.com/XTLS/Xray-core/releases/download/v${XRAY_VERSION}/Xray-linux-64.zip" &&
  unzip -qo x.zip xray) || { echo "::error::the cores could not be downloaded"; exit 1; }

merge() { printf '%s\n' "$1"; cat "$2" 2>/dev/null; }
OWN_SERVERS="$(merge "${OWN_SERVERS:-}" "$tmp/own-servers.txt" | awk 'NF && !seen[$0]++')"
OWN_RELAYS="$(merge "${OWN_RELAYS:-}" "$tmp/own-relays.txt" | awk 'NF && !seen[$0]++')"
while IFS= read -r l; do [ -n "$l" ] && echo "::add-mask::$l"; done <<<"$OWN_SERVERS"$'\n'"$OWN_RELAYS"
export OWN_SERVERS OWN_RELAYS SING_BOX="$tmp/cores/sing-box" XRAY="$tmp/cores/xray"

if ! go run ./cmd/collect -prev "$tmp/prev" -real 15000 -probe-minutes 10 -crowd "$tmp/crowd.json,$tmp/iran.json" \
    -sources "$tmp/sources.txt" -channels "$tmp/channels.txt" -out "$tmp/servers" \
    -keys "$tmp/listkeys.json" -keys-out "$tmp/listkeys.new.json" -plain=false >"$tmp/collect.log" 2>&1; then
  echo "::error::the collection failed"
  grep -E '^(panic|fatal)' "$tmp/collect.log" | tail -3
  exit 1
fi
jq -r '"sources \(.sources), reached \(.reached), found \(.found), tested \(.tested), passed \(.passed); hot \(.files["hot.txt"]), fresh \(.files["fresh.txt"])"' "$tmp/servers/meta.json"

# The key first: a list out before its key reads as empty.
r2_put private/listkeys.json "$tmp/listkeys.new.json" "application/json" "no-store" >/dev/null || { echo "::error::the list key could not be stored"; exit 1; }
r2_put private/source-stats.json "$tmp/servers/source-stats.json" "application/json" "no-store" >/dev/null

# The collector itself, for a tester inside the country (deploy/tester).
mkdir -p "$tmp/tools"
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOARCH=$arch go build -trimpath -buildvcs=false -ldflags "-s -w" -o "$tmp/tools/collect-linux-$arch" ./cmd/collect
done
# Its installer and the cores, so a tester inside the country needs nothing from GitHub.
cp deploy/tester/install.sh "$tmp/tools/tester-install.sh"
cp "$tmp/cores/sing-box" "$tmp/tools/sing-box-linux-amd64"
cp "$tmp/cores/xray" "$tmp/tools/xray-linux-amd64"

account="$(bash build/account-url.sh | cut -d, -f1)"
echo "::add-mask::$account"
files=()
for f in hot.txt proven.txt new.txt fresh.txt all-1.txt all-2.txt all-3.txt all-4.txt all-5.txt all-6.txt all-7.txt all-8.txt own-0.bin own-1.bin own-2.bin own-3.bin own-4.bin; do
  [ -f "$tmp/servers/$f.sealed" ] && files+=("$tmp/servers/$f.sealed")
done
for f in tor.json meta.json iran-tcp.json reach.json; do
  [ -f "$tmp/servers/$f" ] && files+=("$tmp/servers/$f")
done
bash deploy/files/publish-lists.sh "$account" servers "${files[@]}" >/dev/null || { echo "::error::publishing to the project's storage failed"; exit 1; }
bash deploy/files/publish-lists.sh "$account" tools "$tmp"/tools/* >/dev/null
echo "published ${#files[@]} files to the project's storage"

# The public branch: the sealed lists and nothing else, one commit
# replaced each run so the branch never grows.
if [ -n "${PUBLISH_REPO:-}" ] && [ -n "${PUBLISH_TOKEN:-}" ]; then
  rm -rf "$tmp/public" && mkdir -p "$tmp/public"
  cp "$tmp"/servers/*.sealed "$tmp/public/"
  (cd "$tmp/public" &&
    git init -q -b servers &&
    git config user.name "github-actions[bot]" &&
    git config user.email "41898282+github-actions[bot]@users.noreply.github.com" &&
    git add -A && git commit -qm "Lists $(date -u +%Y-%m-%dT%H:%MZ)" &&
    git push -qf "https://x-access-token:${PUBLISH_TOKEN}@github.com/${PUBLISH_REPO}.git" servers) >/dev/null 2>&1 ||
    { echo "::error::the public branch could not be pushed"; exit 1; }
  echo "pushed $(ls "$tmp/public" | wc -l) sealed files"
fi

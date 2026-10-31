#!/usr/bin/env bash
# One run of the tester inside the country (install.sh sets it up):
# fetches the candidates (the current hot.txt and the top of fresh.txt, as
# the collector publishes them: sealed), connects through them for real
# from here, twice, and publishes the report (servers/iran.json) and, when
# enough servers were confirmed, hot.txt. It goes out on the network as it
# is, never through a VPN or a proxy: that network is what it measures.
set -euo pipefail
cd "$(dirname "$0")"
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY all_proxy ALL_PROXY
# The settings install.sh wrote, shell-quoted so any password works.
if [ -f ./env ]; then . ./env; fi
: "${ACCOUNT_URL:?}" "${ADMIN_USER:?}" "${ADMIN_PASSWORD:?}"
account="${ACCOUNT_URL%/}"
work=work
rm -rf "$work" && mkdir -p "$work/in"

# Signed in as the administrator: for the keys that open the sealed lists,
# and later to publish. python3 builds and reads the JSON (no jq needed).
login="$(ADMIN_USER="$ADMIN_USER" ADMIN_PASSWORD="$ADMIN_PASSWORD" python3 -c 'import json, os; print(json.dumps({"username": os.environ["ADMIN_USER"], "password": os.environ["ADMIN_PASSWORD"]}))')"
token="$(curl -fsS --retry 3 --max-time 60 -H 'Content-Type: application/json' --data "$login" \
  "$account/api/v1/admin/login" | python3 -c 'import json, sys; print(json.load(sys.stdin).get("token") or "")')"
[ -n "$token" ] || { echo "could not sign in to the account service" >&2; exit 1; }
(umask 077 && curl -fsS --retry 3 --max-time 60 -H "Authorization: Bearer $token" "$account/api/v1/admin/listkeys" > "$work/keys.json")

# The candidates: whatever the collector abroad found, hot first.
for f in hot.txt fresh.txt; do
  curl -fsS --retry 3 --max-time 300 -o "$work/in/$f.sealed" "$account/list/servers/$f.sealed" || echo "no $f.sealed to test yet" >&2
done
SING_BOX="$PWD/cores/sing-box" XRAY="$PWD/cores/xray" ./collect -hot-from "$work/in/hot.txt.sealed,$work/in/fresh.txt.sealed" \
  -keys "$work/keys.json" -real "${REAL:-600}" -report "$work/iran.json" -tester iran -out "$work"

put() { # name file
  curl -fsS --retry 3 --max-time 300 -X PUT -H "Authorization: Bearer $token" -H 'Content-Type: application/octet-stream' \
    --data-binary @"$2" "$account/api/v1/admin/list/servers/$1" > /dev/null && echo "published servers/$1 ($(stat -c%s "$2") bytes)"
}
put iran.json "$work/iran.json"
# hot.txt from here only when this run confirmed enough servers; a thin run
# leaves the collector's list in place.
confirmed="$(python3 -c 'import json, sys; d = json.load(open(sys.argv[1])); print(sum(1 for v in (d.get("servers") or {}).values() if v and v[0] == 2))' "$work/iran.json")"
if [ -s "$work/hot.txt.sealed" ] && [ "$confirmed" -ge 100 ]; then
  put hot.txt.sealed "$work/hot.txt.sealed"
else
  echo "only $confirmed servers confirmed; hot.txt left to the collector"
fi

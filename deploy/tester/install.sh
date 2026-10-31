#!/usr/bin/env bash
# Sets up a tester inside the country: a machine on an Iranian network that
# connects through the servers for real and reports which ones work from
# here (list/servers/iran.json). The collector abroad cannot see the
# filtering; this machine can. The servers it confirms are proven for the
# accounts (cmd/collect/proven.go) and come first in hot.txt.
#
# One command, on Ubuntu or Debian, also Ubuntu in WSL on Windows, on the
# internet as an ordinary user here has it: no VPN, no proxy.
#
#   curl -fsSL https://accounts.albatrossvpn.online/list/tools/tester-install.sh | sudo bash
#
# It asks for the account service's administrator unless ADMIN_USER and
# ADMIN_PASSWORD are set. Everything it downloads comes from the project's
# own storage; GitHub is only the fallback for the cores. Optional:
#   ACCOUNT_URL=...  the account service (default: the project's)
#   EVERY=6h         how often to test (s, m or h)
#   REAL=450         how many servers to connect through per run (the hot list)
#   SPEED_BYTES=...  bytes each test download carries (default 1 MiB: lines here are metered)
#   CORES_URL=...    another place for the cores' release archives
set -euo pipefail
# What it measures is this network as it is: never through a VPN or proxy.
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY all_proxy ALL_PROXY
ACCOUNT_URL="${ACCOUNT_URL:-https://accounts.albatrossvpn.online}"
ACCOUNT_URL="${ACCOUNT_URL%/}"
EVERY="${EVERY:-6h}"
REAL="${REAL:-450}"
SINGBOX_VERSION="${SINGBOX_VERSION:-1.14.4}"
XRAY_VERSION="${XRAY_VERSION:-26.10.21}"
dir=/opt/albatross-tester
say() { printf '%s\n' "$*"; }
fail() { printf 'Albatross tester: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || fail "run it with sudo (copy the whole command again)."
case "$(uname -m)" in
  x86_64) goarch=amd64; sbarch=amd64; xarch=64 ;;
  aarch64|arm64) goarch=arm64; sbarch=arm64; xarch=arm64-v8a ;;
  *) fail "this machine ($(uname -m)) is not supported; it needs x86-64 or ARM64." ;;
esac
case "$EVERY" in
  *h) every=$(( ${EVERY%h} * 3600 )) ;;
  *m) every=$(( ${EVERY%m} * 60 )) ;;
  *s) every=${EVERY%s} ;;
  *) every=$EVERY ;;
esac
[ "$every" -ge 600 ] 2>/dev/null || fail "EVERY must be at least 10 minutes."

# A base Ubuntu has these; only what is missing is installed.
missing=""
for t in curl python3 tar flock; do command -v "$t" > /dev/null || missing="$missing $t"; done
if [ -n "$missing" ]; then
  say "Installing:$missing"
  pkgs="${missing/flock/util-linux}"
  { apt-get update -q && apt-get install -y -q $pkgs; } > /dev/null 2>&1 || fail "could not install$missing; install them and run the command again."
fi

get() { # url file
  curl -fsSL --retry 5 --retry-delay 3 --connect-timeout 20 --max-time 900 -o "$2" "$1"
}

# The administrator of the account service: the tester signs in with it to
# open the lists and to publish its report. Asked here, kept on this machine.
if [ -z "${ADMIN_USER:-}" ] || [ -z "${ADMIN_PASSWORD:-}" ]; then
  [ -r /dev/tty ] || fail "set ADMIN_USER and ADMIN_PASSWORD (no terminal to ask in)."
  [ -n "${ADMIN_USER:-}" ] || read -r -p "Administrator username: " ADMIN_USER < /dev/tty
  if [ -z "${ADMIN_PASSWORD:-}" ]; then
    read -r -s -p "Administrator password: " ADMIN_PASSWORD < /dev/tty
    printf '\n' > /dev/tty
  fi
fi
login="$(ADMIN_USER="$ADMIN_USER" ADMIN_PASSWORD="$ADMIN_PASSWORD" python3 -c 'import json, os; print(json.dumps({"username": os.environ["ADMIN_USER"], "password": os.environ["ADMIN_PASSWORD"]}))')"
code="$(curl -sS -o /dev/null -w '%{http_code}' --retry 3 --connect-timeout 20 --max-time 60 -H 'Content-Type: application/json' --data "$login" "$ACCOUNT_URL/api/v1/admin/login" || true)"
case "$code" in
  200) say "Signed in to the account service." ;;
  000) fail "the account service cannot be reached from this network right now; try again later." ;;
  *) fail "the account service did not accept that username and password ($code)." ;;
esac

mkdir -p "$dir/cores" "$dir/work"
cd "$dir"

# The collector, built by CI and kept in the project's own storage.
say "Downloading the tester..."
get "$ACCOUNT_URL/list/tools/collect-linux-$goarch" collect || fail "could not download the tester from the account service."
chmod +x collect
./collect -h > /dev/null 2>&1 || fail "the tester does not run on this machine."

# The cores it connects with: from the project's own storage, the release
# archives (GitHub or CORES_URL) only if that fails.
say "Downloading the connection cores..."
if get "$ACCOUNT_URL/list/tools/sing-box-linux-$goarch" cores/sing-box && get "$ACCOUNT_URL/list/tools/xray-linux-$goarch" cores/xray; then
  :
else
  gh="${CORES_URL:-https://github.com}"
  get "$gh/SagerNet/sing-box/releases/download/v${SINGBOX_VERSION}/sing-box-${SINGBOX_VERSION}-linux-${sbarch}.tar.gz" sb.tgz &&
    tar xzf sb.tgz --strip-components=1 -C cores && rm -f sb.tgz &&
    get "$gh/XTLS/Xray-core/releases/download/v${XRAY_VERSION}/Xray-linux-${xarch}.zip" x.zip &&
    python3 -c 'import sys, zipfile; zipfile.ZipFile(sys.argv[1]).extract("xray", sys.argv[2])' x.zip cores && rm -f x.zip ||
    fail "could not download the connection cores."
fi
chmod +x cores/sing-box cores/xray
cores/sing-box version > /dev/null 2>&1 || fail "the sing-box core does not run on this machine."

# The settings, for run.sh: shell-quoted, so a password with any
# character in it reaches the account service as it is, and readable by
# root only from the start.
(umask 077 && {
  printf 'ACCOUNT_URL=%q\n' "$ACCOUNT_URL"
  printf 'ADMIN_USER=%q\n' "$ADMIN_USER"
  printf 'ADMIN_PASSWORD=%q\n' "$ADMIN_PASSWORD"
  printf 'REAL=%q\n' "$REAL"
  [ -n "${SPEED_BYTES:-}" ] && printf 'SPEED_BYTES=%q\n' "$SPEED_BYTES"
} > ./env)

# One run (deploy/tester/run.sh in the repository; CI checks the two match).
cat > run.sh <<'RUN'
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
# FULL=1 (the first run after installing): every server the collector
# found, not only hot.txt.
[ "${FULL:-0}" = 1 ] && REAL=100000
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

# The candidates: whatever the collector abroad found, hot first; the top
# of fresh.txt only when a run tests more than hot.txt holds (450), which
# saves a metered line the download.
lists="hot.txt"
[ "${REAL:-450}" -gt 450 ] && lists="hot.txt fresh.txt"
for f in $lists; do
  curl -fsS --retry 3 --max-time 300 -o "$work/in/$f.sealed" "$account/list/servers/$f.sealed" || echo "no $f.sealed to test yet" >&2
done
from="$work/in/hot.txt.sealed"
[ -s "$work/in/fresh.txt.sealed" ] && from="$from,$work/in/fresh.txt.sealed"
SING_BOX="$PWD/cores/sing-box" XRAY="$PWD/cores/xray" ./collect -hot-from "$from" \
  -keys "$work/keys.json" -real "${REAL:-450}" -speed-bytes "${SPEED_BYTES:-1048576}" -report "$work/iran.json" -tester iran -out "$work"

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
RUN
chmod +x run.sh

# Runs every $EVERY, one at a time, keeping the last part of its log.
cat > loop.sh <<LOOP
#!/usr/bin/env bash
cd $dir
exec 9> $dir/loop.lock
flock -n 9 || exit 0
while true; do
  # The first run tests every server the collector found; the rest, hot.txt.
  full=1; [ -f first.done ] && full=0
  { date -u '+%F %T UTC: run'; FULL=\$full ./run.sh && touch first.done; } >> log.txt 2>&1 || true
  tail -n 3000 log.txt > log.tmp && mv log.tmp log.txt
  sleep $every
done
LOOP
chmod +x loop.sh

if grep -qi microsoft /proc/version 2>/dev/null; then
  # Ubuntu in WSL on Windows: Windows starts it at sign-in, hidden, and the
  # running loop keeps this Linux awake. No administrator rights needed.
  win=/mnt/c/Windows/System32
  distro="$(wslpath -w / 2>/dev/null | tr -d '\r' | sed -E 's#^\\\\wsl(\.localhost|\$)\\([^\\]+).*#\2#')"
  [ -n "$distro" ] || fail "could not tell which WSL distribution this is."
  appdata="$(cd /mnt/c && "$win/cmd.exe" /c 'echo %APPDATA%' 2>/dev/null | tr -d '\r')"
  [ -n "$appdata" ] || fail "could not reach Windows from this Linux (WSL interop is off)."
  startup="$(wslpath -u "$appdata")/Microsoft/Windows/Start Menu/Programs/Startup"
  mkdir -p "$startup"
  printf 'CreateObject("WScript.Shell").Run "wsl.exe -d %s -u root -- %s/loop.sh", 0, False\r\n' "$distro" "$dir" > "$startup/Albatross tester.vbs"
  (cd /mnt/c && "$win/wscript.exe" "$(wslpath -w "$startup/Albatross tester.vbs")") || fail "could not start the tester in the background."
  say "Installed. It runs now and every $EVERY, and starts again whenever you sign in to Windows."
  say "Keep the computer on and awake (Windows: Settings > System > Power > Sleep: Never)."
elif [ "$(ps -p 1 -o comm= 2>/dev/null)" = systemd ]; then
  cat > /etc/systemd/system/albatross-tester.service <<UNIT
[Unit]
Description=Albatross tester inside the country
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$dir/loop.sh
Restart=always
RestartSec=60
Nice=10

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now albatross-tester.service
  say "Installed. It runs now and every $EVERY, also after a restart."
else
  (crontab -l 2>/dev/null | grep -v albatross-tester; echo "@reboot $dir/loop.sh") | crontab - 2>/dev/null || true
  setsid nohup "$dir/loop.sh" > /dev/null 2>&1 < /dev/null &
  say "Installed. It runs now and every $EVERY."
fi
say "Its log: sudo tail -f $dir/log.txt"

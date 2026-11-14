#!/bin/sh
# Starts the WARP daemon, registers once, switches to proxy mode and republishes
# the proxy on 0.0.0.0:1080. Registration lives in /var/lib/cloudflare-warp, which
# compose keeps in a volume, so restarts do not register a new device each time.
set -eu

PORT=40000
warp() { warp-cli --accept-tos "$@"; }

mkdir -p /run/dbus
rm -f /run/dbus/pid
dbus-daemon --system --fork

warp-svc --accept-tos &
SVC_PID=$!

# The daemon needs a moment before warp-cli can talk to it.
i=0
until warp status >/dev/null 2>&1; do
  i=$((i + 1))
  [ "$i" -le 30 ] || { echo "warp-svc did not start" >&2; exit 1; }
  sleep 1
done

if ! warp registration show >/dev/null 2>&1; then
  echo "registering a new WARP device"
  warp registration new
fi

warp mode proxy
warp proxy port "$PORT"
warp connect

socat TCP-LISTEN:1080,fork,reuseaddr "TCP:127.0.0.1:$PORT" &

echo "WARP proxy ready on :1080"
wait "$SVC_PID"

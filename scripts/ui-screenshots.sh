#!/usr/bin/env bash
# Screenshots of the UI in every state worth reviewing, desktop and phone,
# light and dark, from the preview server (made-up data; no cluster).
#
#   scripts/ui-screenshots.sh [out-dir]     # default: ./screenshots
#
# Needs Go, jq and Google Chrome (headless). CHROME overrides the binary.
set -euo pipefail

out=${1:-screenshots}
port=${PREVIEW_PORT:-8091}
chrome=${CHROME:-}
if [[ -z $chrome ]]; then
	for c in "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" google-chrome chromium chromium-browser; do
		if command -v "$c" >/dev/null 2>&1 || [[ -x $c ]]; then chrome=$c; break; fi
	done
fi
[[ -n $chrome ]] || { echo "no Chrome found; set CHROME" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'kill "$server" 2>/dev/null || true; rm -rf "$tmp"' EXIT
go build -o "$tmp/alert-agent" ./cmd/alert-agent
PREVIEW_ADDR=127.0.0.1:$port "$tmp/alert-agent" ui-preview >"$tmp/server.log" 2>&1 &
server=$!
for _ in $(seq 50); do
	curl -fsS -o /dev/null "http://127.0.0.1:$port/healthz" 2>/dev/null && break
	sleep 0.1
done
mkdir -p "$out"

# shot NAME DEVICE SCHEME PATH [HEIGHT]
# HEIGHT makes the page that tall, to show what is below the fold (headless
# Chrome screenshots before it scrolls to a #fragment).
shot() {
	local name=$1 device=$2 scheme=$3 path=$4 height=${5:-} size scale pref url
	url="http://127.0.0.1:$port$path"
	case $device in
		desktop) size=1440,${height:-900} scale=1 ;;
		# Desktop Chrome will not lay a window out narrower than ~500px:
		# the preview frames the page at a phone's size instead.
		phone) size=390,${height:-844} scale=2
			url="http://127.0.0.1:$port/__phone?h=${height:-844}&src=$(printf %s "$path" | jq -sRr @uri)" ;;
	esac
	case $scheme in
		light) pref=1 ;;
		dark) pref=0 ;;
	esac
	"$chrome" --headless=new --disable-gpu --hide-scrollbars --no-first-run --user-data-dir="$tmp/profile-$name-$device-$scheme" \
		--window-size=$size --force-device-scale-factor=$scale --blink-settings=preferredColorScheme=$pref \
		--timeout=2500 --screenshot="$out/$name-$device-$scheme.png" "$url" >/dev/null 2>&1
	echo "$out/$name-$device-$scheme.png"
}

for device in desktop phone; do
	shot home-closed "$device" light "/"
	shot incident-closed "$device" light "/incidents/25"
	shot incident-open "$device" light "/incidents/25?chat=open&thread=1"
	shot switcher "$device" light "/incidents/25?chat=open&thread=1&menu=1"
	shot other-thread "$device" light "/incidents/26?chat=open&thread=2"
	shot incident-thread-on-home "$device" light "/?chat=open&thread=1"
	shot streaming "$device" light "/?chat=open&thread=4"
	shot new-general "$device" light "/?chat=open&pending=general"
	shot new-incident "$device" light "/incidents/27?chat=open"
	shot read-only-capped "$device" light "/incidents/24?chat=open&thread=5"
	shot run "$device" light "/runs/29"
	# Dark: the pages themselves, then the chat window's states.
	shot home-closed "$device" dark "/"
	shot incident-closed "$device" dark "/incidents/25"
	shot run "$device" dark "/runs/29"
	shot incident-open "$device" dark "/incidents/25?chat=open&thread=1"
	shot switcher "$device" dark "/incidents/25?chat=open&thread=1&menu=1"
	shot other-thread "$device" dark "/incidents/26?chat=open&thread=2"
	shot streaming "$device" dark "/?chat=open&thread=4"
	shot new-general "$device" dark "/?chat=open&pending=general"
	shot read-only-capped "$device" dark "/incidents/24?chat=open&thread=5"
	# Phase 5: approvals, actions and the pause switch, light and dark.
	for scheme in light dark; do
		shot home-paused "$device" $scheme "/?paused=1"
		shot approval-pending "$device" $scheme "/incidents/27"
		shot approval-decided "$device" $scheme "/incidents/29"
		shot autonomous-action "$device" $scheme "/incidents/28"
		shot activity-autonomous "$device" $scheme "/incidents/28" 2600
		shot activity-escalated "$device" $scheme "/incidents/29" 3000
		shot approval-expired "$device" $scheme "/incidents/24"
		shot approval-rejected "$device" $scheme "/incidents/23"
		shot chat-approval-pending "$device" $scheme "/incidents/27?chat=open&thread=7"
		shot chat-approval-decided "$device" $scheme "/incidents/29?chat=open&thread=6"
	done
done
# Phones are always full screen; on a desktop it is a toggle.
shot fullscreen desktop light "/incidents/25?chat=open&full=1&thread=1"
shot fullscreen desktop dark "/incidents/25?chat=open&full=1&thread=1"

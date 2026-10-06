#!/usr/bin/env bash
# Doctors on call: the README quick start, including the seed it assumes.
# Both transactions commit. The final on_call=true scan is empty.
set -euo pipefail
set +m

root=$(cd "$(dirname "$0")/../.." && pwd)
data=$(mktemp -d)
addr=${ADDR:-127.0.0.1:18080}
base="http://$addr"

go run "$root/cmd/marchisql" -addr="$addr" -storageDataPath="$data" >/tmp/marchisql-doctors.log 2>&1 &
pid=$!
cleanup() {
	kill "$pid" 2>/dev/null || true
	wait "$pid" 2>/dev/null || true
	rm -rf "$data"
}
trap cleanup EXIT

for _ in $(seq 1 50); do
	curl -sf "$base/healthz" >/dev/null && break
	sleep 0.2
done

txid() { curl -sf -X POST "$base/begin" | python3 -c 'import json,sys; print(json.load(sys.stdin)["txid"])'; }
count() { curl -sf "$1" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))'; }

seed=$(txid)
curl -sf -X POST "$base/update" -d "{\"tx\":\"$seed\",\"key\":\"alice\",\"on_call\":true}" >/dev/null
curl -sf -X POST "$base/update" -d "{\"tx\":\"$seed\",\"key\":\"bob\",\"on_call\":true}" >/dev/null
curl -sf -X POST "$base/commit" -d "{\"tx\":\"$seed\"}" >/dev/null

A=$(txid)
echo "A sees $(count "$base/scan?tx=$A&where=on_call=true") on call"
B=$(txid)
echo "B sees $(count "$base/scan?tx=$B&where=on_call=true") on call"

curl -sf -X POST "$base/update" -d "{\"tx\":\"$B\",\"key\":\"bob\",\"on_call\":false}" >/dev/null
curl -sf -X POST "$base/commit" -d "{\"tx\":\"$B\"}" >/dev/null
curl -sf -X POST "$base/update" -d "{\"tx\":\"$A\",\"key\":\"alice\",\"on_call\":false}" >/dev/null
curl -sf -X POST "$base/commit" -d "{\"tx\":\"$A\"}" >/dev/null

left=$(count "$base/scan?where=on_call=true")
echo "on call after both commits: $left"
test "$left" -eq 0

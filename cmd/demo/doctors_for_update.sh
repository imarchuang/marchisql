#!/usr/bin/env bash
# Doctors on call, with FOR UPDATE on the on-call set.
# The second transaction's scan conflicts. It aborts. One doctor stays on call.
set -euo pipefail
set +m

root=$(cd "$(dirname "$0")/../.." && pwd)
data=$(mktemp -d)
addr=${ADDR:-127.0.0.1:18082}
base="http://$addr"

go run "$root/cmd/marchisql" -addr="$addr" -storageDataPath="$data" >/tmp/marchisql-doctors-for-update.log 2>&1 &
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
echo "A locks $(count "$base/scan?tx=$A&where=on_call=true&for_update=true") on call"
B=$(txid)
code=$(curl -s -o /tmp/marchisql-for-update-b.json -w '%{http_code}' \
	"$base/scan?tx=$B&where=on_call=true&for_update=true")
echo "B for update: HTTP $code"
test "$code" -eq 409

curl -sf -X POST "$base/abort" -d "{\"tx\":\"$B\"}" >/dev/null
curl -sf -X POST "$base/update" -d "{\"tx\":\"$A\",\"key\":\"alice\",\"on_call\":false}" >/dev/null
curl -sf -X POST "$base/commit" -d "{\"tx\":\"$A\"}" >/dev/null

left=$(count "$base/scan?where=on_call=true")
echo "on call after A commits and B aborts: $left"
test "$left" -eq 1

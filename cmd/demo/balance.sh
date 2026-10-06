#!/usr/bin/env bash
# Two accounts, constraint balance(a)+balance(b) >= 0.
# Each transaction withdraws 200 from a different account. Both commit.
# The sum is -200.
set -euo pipefail
set +m

root=$(cd "$(dirname "$0")/../.." && pwd)
data=$(mktemp -d)
addr=${ADDR:-127.0.0.1:18081}
base="http://$addr"

go run "$root/cmd/marchisql" -addr="$addr" -storageDataPath="$data" >/tmp/marchisql-balance.log 2>&1 &
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
bal() {
	curl -sf "$base/get?key=$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["fields"]["balance"])'
}

seed=$(txid)
curl -sf -X POST "$base/update" -d "{\"tx\":\"$seed\",\"key\":\"a\",\"balance\":100}" >/dev/null
curl -sf -X POST "$base/update" -d "{\"tx\":\"$seed\",\"key\":\"b\",\"balance\":100}" >/dev/null
curl -sf -X POST "$base/commit" -d "{\"tx\":\"$seed\"}" >/dev/null

A=$(txid)
B=$(txid)
echo "A reads sum $(($(bal a) + $(bal b)))"
# Each withdrawal is legal against its own snapshot.
curl -sf -X POST "$base/update" -d "{\"tx\":\"$A\",\"key\":\"a\",\"balance\":-100}" >/dev/null
curl -sf -X POST "$base/commit" -d "{\"tx\":\"$A\"}" >/dev/null
curl -sf -X POST "$base/update" -d "{\"tx\":\"$B\",\"key\":\"b\",\"balance\":-100}" >/dev/null
curl -sf -X POST "$base/commit" -d "{\"tx\":\"$B\"}" >/dev/null

sum=$(($(bal a) + $(bal b)))
echo "sum after both withdrawals: $sum"
test "$sum" -eq -200

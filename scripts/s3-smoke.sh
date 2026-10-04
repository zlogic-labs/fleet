# Run Fleet's S3 blobstore against the object store on this machine.
#
# The point is the code path, not the server. blobstore.S3 has been compiled and
# cross-compiled since it was written but has only ever run against itself in
# tests, so "it works" has meant "the code is plausible". This drives the same
# gateway the console reads, and asserts on what the operator would see.
set -uo pipefail

BASE=${FLEET_S3_ENDPOINT:-http://127.0.0.1:9000}
CTRL=${CTRL:-http://172.31.74.144:8081/api/v1}
TOKEN=${TOKEN:-$(cat "$HOME/fleet/state/admin.token" 2>/dev/null)}
KEY=""
PASS=0
FAIL=0

ok()   { PASS=$((PASS+1)); printf '  ok   %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL %s\n     %s\n' "$1" "${2:-}"; }

need() { [ -n "$2" ] && ok "$1" || bad "$1" "${2:-empty}"; }

auth=(-H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json')

echo "== reachability"
# An unauthenticated GET / on an S3 endpoint answers 403, which is the store
# working: it is alive and refusing. Treating that as "down" was a bug in this
# script, not in the store.
code=$(curl -sS -o /dev/null -w '%{http_code}' -m 8 "$BASE" 2>/dev/null)
case "$code" in
  403|400) ok "the object store answers and refuses anonymous access ($code)";;
  200)     ok "the object store answers ($code)";;
  000)     bad "the object store answers" "no response from $BASE";;
  *)       bad "the object store answers" "http $code";;
esac

echo "== storage before anything is written"
d=$(curl -fsS -m 10 "${auth[@]}" "$CTRL/storage" 2>/dev/null)
echo "$d" | grep -q '"reachable":true' && ok "Fleet reports the bucket reachable" \
  || bad "Fleet reports the bucket reachable" "$d"
echo "$d" | grep -q 'fleet-weights' && ok "the bucket name round-trips" \
  || bad "the bucket name round-trips" "$d"
echo "$d" | grep -q '"endpoint":"127.0.0.1:9000' && ok "the endpoint is the S3 one, not the local directory" \
  || bad "the endpoint is the S3 one, not the local directory" "$d"

echo "== a pull that has to write objects"
p=$(curl -fsS -m 20 -X POST "${auth[@]}" -d '{"model":"bartowski/Qwen2.5-0.5B-Instruct-GGUF","engine":"llama-cpp"}' \
  "$CTRL/pulls" 2>/dev/null)
pid=$(printf '%s' "$p" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$pid" ] && ok "pull accepted ($pid)" || bad "pull accepted" "$p"

for _ in $(seq 1 40); do
  s=$(curl -fsS -m 10 "${auth[@]}" "$CTRL/pulls/$pid" 2>/dev/null)
  st=$(printf '%s' "$s" | sed -n 's/.*"state":"\([^"]*\)".*/\1/p')
  case "$st" in done|failed|canceled) break;; esac
  sleep 2
done

echo "== the objects are actually in S3"
d=$(curl -fsS -m 10 "${auth[@]}" "$CTRL/storage" 2>/dev/null)
objs=$(printf '%s' "$d" | sed -n 's/.*"objectCount":\([0-9]*\).*/\1/p')
used=$(printf '%s' "$d" | sed -n 's/.*"usedBytes":\([0-9]*\).*/\1/p')
[ "${objs:-0}" -gt 0 ] && ok "objects counted ($objs, $used bytes)" || bad "objects counted" "$d"
[ "${used:-0}" -gt 0 ] && ok "bytes counted" || bad "bytes counted" "$d"

# This repository is 9.85 GB, so waiting for done is waiting for a long download
# and proves nothing more than the write path above. What matters is that the
# pull can be stopped and lands in a state that says so rather than being
# reported as a failure.
if [ "$st" = done ]; then
  ok "pull reached done"
else
  curl -fsS -m 10 -X PATCH "${auth[@]}" -d '{"state":"canceled"}' "$CTRL/pulls/$pid" >/dev/null 2>&1
  sleep 3
  c=$(curl -fsS -m 10 "${auth[@]}" "$CTRL/pulls/$pid" 2>/dev/null)
  printf '%s' "$c" | grep -q '"state":"canceled"' && ok "a running pull cancels cleanly" \
    || bad "a running pull cancels cleanly" "$c"
  printf '%s' "$c" | grep -q '"state":"failed"' && bad "a canceled pull is not reported as failed" "$c" \
    || ok "a canceled pull is not reported as failed"
fi

echo "== verify distinguishes three failures, not one"
# This pull was cancelled, so its weights are not stored: that is a state the
# caller can wait out, which is what 409 means. 404 is a name nobody registered
# and 400 is a request that cannot be satisfied as written.
vc=$(curl -sS -m 15 -o /tmp/v.json -w '%{http_code}' "${auth[@]}" \
  "$CTRL/repositories/bartowski/Qwen2.5-0.5B-Instruct-GGUF?engine=llama-cpp" 2>/dev/null)
[ "$vc" = 409 ] && ok "a registered model whose weights are missing answers 409" \
  || bad "a registered model whose weights are missing answers 409" "got $vc: $(head -c 200 /tmp/v.json)"

vn=$(curl -sS -m 15 -o /dev/null -w '%{http_code}' "${auth[@]}" \
  "$CTRL/repositories/nobody/nothing-here?engine=llama-cpp" 2>/dev/null)
[ "$vn" = 404 ] && ok "an unregistered name answers 404" \
  || bad "an unregistered name answers 404" "got $vn"

# The engine-mismatch branch (400) is deliberately not checked here. Verify
# tests readiness before it tests engine compatibility, so a model whose weights
# are missing answers 409 whichever engine is named -- and producing a ready
# GGUF entry needs a completed pull of several gigabytes. That case is covered
# where the state can be constructed, in repository_status_test.go.

echo "== a bad credential is refused by the store, not swallowed"
curl -fsS -m 10 -o /dev/null "${auth[@]/Bearer $TOKEN/X-Api-Key nope}" \
  "$CTRL/storage" 2>/dev/null && bad "Fleet rejects a wrong admin token" "it answered 200" \
  || ok "Fleet rejects a wrong admin token"

echo
echo "PASS $PASS, FAIL $FAIL"
[ "$FAIL" -eq 0 ]
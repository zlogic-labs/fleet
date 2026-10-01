#!/usr/bin/env bash
# End-to-end checks against a running dev stack.
#
#   ./scripts/dev.sh          # in one terminal
#   ./scripts/smoke.sh        # in another
#
# Exits non-zero on the first failure, so it is usable as a gate and not only as
# something to read. Every check here is an assertion about behaviour that used
# to be wrong, not a liveness ping: a server that answers 200 while rendering
# a finished download as 1% passes a ping and fails this.
#
# Nothing here needs a GPU, network access or object storage.

set -uo pipefail

GATEWAY=${GATEWAY:-http://127.0.0.1:8080}
CONTROL=${CONTROL:-http://127.0.0.1:8081}
API=$CONTROL/api/v1

pass=0
fail=0

# Scratch space for this run's own artifacts. It is created here rather than
# inherited because TMPDIR is routinely unset, and an unset TMPDIR turns a log
# path into "/auth-gateway.log" — a write to the filesystem root that fails on
# any machine not running as root, which then reads as the gateway refusing to
# start.
WORKDIR=$(mktemp -d 2>/dev/null || echo "${TMPDIR:-/tmp}/fleet-smoke.$$")
mkdir -p "$WORKDIR"

# One cleanup for the whole script. Each trap would replace the previous one, so
# a second trap set later in the file silently cancels the first and the
# temporary directory survives every run.
cleanup() {
  for pid in "${AUTH_PID:-}" "${TOKEN_PID:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null
  done
  rm -rf "$WORKDIR"
  return 0
}
trap cleanup EXIT INT TERM

# Resolved once, up front. Ubuntu ships python3 with no "python" shim, and a
# script that assumes the name fails every check while reporting dozens of
# assertion failures that have nothing to do with the code under test — which
# is worse than no test, because it looks like a regression.
#
# Each candidate is executed, not merely located. `command -v` is not enough:
# Windows installs an App Execution Alias named python3 that resolves on PATH
# and then fails when run, so detection passes and every JSON check afterwards
# fails on a missing interpreter. Locating the interpreter is only evidence if
# the interpreter answers.
PY=""
for candidate in python3 python; do
  if command -v "$candidate" >/dev/null 2>&1 && "$candidate" -c "" >/dev/null 2>&1; then
    PY=$candidate
    break
  fi
done
if [ -z "$PY" ]; then
  printf '%s no working python3 or python on PATH; cannot run the JSON checks\n' "$(red 'ERROR')"
  exit 2
fi

green() { printf '\033[32m%s\033[0m' "$1"; }
red()   { printf '\033[31m%s\033[0m' "$1"; }

section() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# check NAME EXPECTED ACTUAL
check() {
  if [ "$2" = "$3" ]; then
    pass=$((pass + 1))
    printf '  %s %s\n' "$(green 'ok  ')" "$1"
  else
    fail=$((fail + 1))
    printf '  %s %s\n        expected: %s\n        actual:   %s\n' \
      "$(red 'FAIL')" "$1" "$2" "$3"
  fi
}

# Fails loudly rather than printing an empty string. An expression that
# indexes past the end of a list raises, and a bare "" would compare equal to
# an expected "" — so a missing entry would silently pass the check that was
# supposed to prove it exists.
jqp() {
  "$PY" -c "
import json,sys
d=json.load(sys.stdin)
try:
    print(eval(sys.argv[1],{'d':d}))
except Exception as e:
    print('<<error: %s>>' % e)
" "$1"
}

section "waiting for both processes"
for _ in $(seq 1 60); do
  if curl -fsS "$GATEWAY/healthz" >/dev/null 2>&1 &&
     curl -fsS "$CONTROL/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.5
done
if ! curl -fsS "$GATEWAY/healthz" >/dev/null 2>&1; then
  red "no gateway at $GATEWAY — start ./scripts/dev.sh" >&2
  exit 1
fi
if ! curl -fsS "$CONTROL/healthz" >/dev/null 2>&1; then
  red "no control plane at $CONTROL — start ./scripts/dev.sh" >&2
  exit 1
fi
echo "  gateway $GATEWAY, control plane $CONTROL"

# ── the gateway is a real OpenAI-compatible endpoint ────────────────
section "gateway: OpenAI compatibility"

BODY='{"model":"demo/Qwen2.5-1.5B-Instruct","messages":[{"role":"user","content":"hello"}],"stream":false,"stream_options":{"include_usage":true}}'
R=$(curl -sS -X POST "$GATEWAY/v1/chat/completions" -H 'Content-Type: application/json' -d "$BODY")
check "non-stream returns choices[0].message.content" "True" \
  "$(printf '%s' "$R" | jqp "bool(d['choices'][0]['message'].get('content'))")"
# P6: the gateway must trust the engine's usage, not invent it. Zero means
# the tap missed the usage frame, which is the bug that makes everything
# unbillable.
check "non-stream reports a nonzero usage" "True" \
  "$(printf '%s' "$R" | jqp "d['usage']['total_tokens'] > 0")"
check "usage total equals prompt + completion" "True" \
  "$(printf '%s' "$R" | jqp "d['usage']['total_tokens'] == d['usage']['prompt_tokens'] + d['usage']['completion_tokens']")"

SBODY='{"model":"demo/Qwen2.5-1.5B-Instruct","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}'
S=$(curl -sS -N -X POST "$GATEWAY/v1/chat/completions" -H 'Content-Type: application/json' -d "$SBODY")
check "stream emits more than one data frame" "True" \
  "$(printf '%s\n' "$S" | grep -c '^data: ' | "$PY" -c 'import sys;print(int(sys.stdin.read()) > 1)')"
check "stream ends with [DONE]" "True" \
  "$(printf '%s\n' "$S" | grep -q 'data: \[DONE\]' && echo True || echo False)"
check "stream carries a usage frame" "True" \
  "$(printf '%s\n' "$S" | grep '^data: ' | sed 's/^data: //' \
     | "$PY" -c "
import sys,json
for line in sys.stdin:
    line=line.strip()
    if not line or line=='[DONE]': continue
    try: d=json.loads(line)
    except ValueError: continue
    if d.get('usage'): print('True'); break
else: print('False')")"

check "GET /v1/models lists a model" "True" \
  "$(curl -sS "$GATEWAY/v1/models" | jqp "len(d['data']) > 0")"
check "GET /fleet/status reports the edition" "community" \
  "$(curl -sS "$GATEWAY/fleet/status" | jqp "d['edition']")"

# A 4xx from the engine must reach the client as an OpenAI error envelope,
# not as chi's plain-text 404 page.
check "an unknown model returns an error envelope" "True" \
  "$(curl -sS -X POST "$GATEWAY/v1/chat/completions" -H 'Content-Type: application/json' \
     -d '{"model":"nope","messages":[]}' \
     | jqp "'error' in d and 'type' in d['error']")"

# ── the console is embedded and its assets resolve ──────────────────
section "console"

check "GET / returns the app shell" "True" \
  "$(curl -sS "$GATEWAY/" | grep -q '<div id="root">' && echo True || echo False)"
# A deep link must serve the shell too, or a refresh on /models 404s.
check "a deep link returns the app shell" "True" \
  "$(curl -sS "$GATEWAY/models" | grep -q '<div id="root">' && echo True || echo False)"

# Every asset the shell references must actually be served, with the right
# content type. A 200 that is really index.html is the failure that hides.
ASSET=$(curl -sS "$GATEWAY/" | grep -o '/assets/[^"]*\.js' | head -1)
if [ -n "$ASSET" ]; then
  check "asset $ASSET is served as javascript" "True" \
    "$(curl -sS -o /dev/null -w '%{content_type}' "$GATEWAY$ASSET" \
       | grep -q 'javascript' && echo True || echo False)"
  check "asset $ASSET is not the app shell" "True" \
    "$(curl -sS "$GATEWAY$ASSET" | grep -q '<div id="root">' && echo False || echo True)"
else
  fail=$((fail + 1))
  printf '  %s the shell references no /assets/*.js — the console was not built (make web)\n' "$(red 'FAIL')"
fi

# ── the control plane ──────────────────────────────────────────────
section "control plane: routes"

for route in models pulls storage engines cluster deployments; do
  check "GET /api/v1/$route" "200" \
    "$(curl -sS -o /dev/null -w '%{http_code}' "$API/$route")"
done
# CORS: the console is served from :8080 and the control plane answers on
# :8081, so without this every request is a browser error.
check "a loopback origin is allowed" "True" \
  "$(curl -sS -o /dev/null -D - -H 'Origin: http://127.0.0.1:8080' \
     "$API/models" | grep -qi 'access-control-allow-origin: http://127.0.0.1:8080' \
     && echo True || echo False)"
# Substring matching would let a hostile origin through.
check "a foreign origin is not allowed" "False" \
  "$(curl -sS -o /dev/null -D - -H 'Origin: https://evil.example.com' \
     "$API/models" | grep -qi 'access-control-allow-origin' \
     && echo True || echo False)"
check "an unknown model returns an error envelope" "True" \
  "$(curl -sS "$API/models/definitely%2Fnot-here" \
     | jqp "'error' in d and 'type' in d['error']")"

# ── engine profiles: the thing this change was about ────────────────
section "engine profiles"

E=$(curl -sS "$API/engines")
check "vllm is a known engine" "True" \
  "$(printf '%s' "$E" | jqp "any(e['name']=='vllm' for e in d)")"
check "llama-cpp is a known engine" "True" \
  "$(printf '%s' "$E" | jqp "any(e['name']=='llama-cpp' for e in d)")"
# The whole design rests on this: vLLM refuses an older card, llama.cpp does not.
check "vllm requires compute 7.5+" "75" \
  "$(printf '%s' "$E" | jqp "[e['minCompute'] for e in d if e['name']=='vllm'][0]")"
check "llama-cpp claims no compute floor" "0" \
  "$(printf '%s' "$E" | jqp "[e['minCompute'] for e in d if e['name']=='llama-cpp'][0]")"
# llama-server does publish Prometheus metrics under its own llamacpp: prefix,
# verified against its README. What it must NOT claim is a cache-occupancy
# signal: llamacpp:n_tokens_max is an observed high-water mark of context size,
# and an autoscaler reading that as occupancy would never scale down an engine
# that is nearly empty.
check "llama-cpp declares autoscaling metrics" "True" \
  "$(printf '%s' "$E" | jqp "[e['metrics'] for e in d if e['name']=='llama-cpp'][0]")"
check "llama-cpp claims no KV cache occupancy signal" "True" \
  "$(printf '%s' "$E" | jqp "[not e['kvCacheUsage'] for e in d if e['name']=='llama-cpp'][0]")"
check "vllm maps KV cache occupancy to a real series" "vllm:kv_cache_usage_perc" \
  "$(printf '%s' "$E" | jqp "[e['kvCacheUsage'] for e in d if e['name']=='vllm'][0]")"
check "llama-cpp publishes no KV cache capacity" "False" \
  "$(printf '%s' "$E" | jqp "[e['capacity'] for e in d if e['name']=='llama-cpp'][0]")"
# vLLM does report capacity, as labels on an info gauge whose value is always 1.
check "vllm publishes KV cache capacity" "True" \
  "$(printf '%s' "$E" | jqp "[e['capacity'] for e in d if e['name']=='vllm'][0]")"
check "vllm declares autoscaling metrics" "True" \
  "$(printf '%s' "$E" | jqp "[e['metrics'] for e in d if e['name']=='vllm'][0]")"
check "llama-cpp declares no engine tokenizer" "False" \
  "$(printf '%s' "$E" | jqp "[e['tokenize'] for e in d if e['name']=='llama-cpp'][0]")"

# ── weight formats ─────────────────────────────────────────────────
section "weight formats"

pull() {
  curl -sS -o /dev/null -X POST "$API/pulls" -H 'Content-Type: application/json' -d "$1"
}
# The registry key comes from sourceRef, not from the model field, so each of
# these needs its own repository to be a distinct entry.
pull '{"model":"Qwen/Qwen2.5-1.5B-Instruct","source":"huggingface","sourceRef":"Qwen/Qwen2.5-1.5B-Instruct","engine":"vllm"}'
pull '{"model":"bartowski/Qwen2.5-0.5B-Instruct-GGUF","source":"huggingface","sourceRef":"bartowski/Qwen2.5-0.5B-Instruct-GGUF","engine":"llama-cpp"}'
# A repository whose name matches no encoding prefix, where the only way to get
# an exact count is for the operator to pin the encoding by hand.
pull '{"model":"fleet/pinned-encoding","source":"huggingface","sourceRef":"fleet/pinned-encoding","engine":"vllm","tokenizerId":"o200k_base"}'

echo "  waiting for pulls to settle"
for _ in $(seq 1 80); do
  n=$(curl -sS "$API/pulls" | grep -c '"state":"\(queued\|running\)"' || true)
  [ "$n" = "0" ] && break
  sleep 0.25
done

M=$(curl -sS "$API/models")
ST=$(curl -sS "$API/pulls")

check "the safetensors repository is detected as safetensors" "safetensors" \
  "$(printf '%s' "$M" | jqp "[m['format'] for m in d if m['name']=='Qwen/Qwen2.5-1.5B-Instruct'][0]")"
check "the GGUF repository is detected as gguf" "gguf" \
  "$(printf '%s' "$M" | jqp "[m['format'] for m in d if m['name'].endswith('GGUF')][0]")"
# GGUF carries its tokenizer inside the weights file. An empty id is the correct
# value; a repository name here is a claim the engine will ignore.
check "a GGUF model records no external tokenizer" "" \
  "$(printf '%s' "$M" | jqp "[m['tokenizerId'] for m in d if m['name'].endswith('GGUF')][0]")"
# The tokenizer field is a tiktoken encoding name, not a model name. The
# gateway's resolver returns a non-empty hint verbatim and skips prefix
# matching, so a guess stored here suppresses the exact table — and the token
# count is what billing settles on (P5, P6).
check "Fleet does not invent a tokenizer id" "" \
  "$(printf '%s' "$M" | jqp "[m['tokenizerId'] for m in d if m['name']=='Qwen/Qwen2.5-1.5B-Instruct'][0]")"
check "an operator-pinned encoding survives the pull" "o200k_base" \
  "$(printf '%s' "$M" | jqp "[m['tokenizerId'] for m in d if m['name']=='fleet/pinned-encoding'][0]")"
# The invariant, checked across every entry rather than one model: a tokenizer
# id is either empty (let the resolver infer from the model name) or a name
# tiktoken actually has a table for. Anything else fails to load and silently
# degrades to the estimator, which is a billing error, not a display one.
check "every tokenizer id is a real encoding name" "True" \
  "$(printf '%s' "$M" | jqp "all(t in {'o200k_base','cl100k_base','p50k_base','r50k_base','gpt2'} or t == '' for t in [m['tokenizerId'] for m in d])")"
# Progress is a fraction in [0,1] on the wire. A finished job is 1.0, and a
# console that passes it through unscaled renders it as 1%.
check "a finished pull reports progress 1.0" "True" \
  "$(printf '%s' "$ST" | jqp "all(p['progress']==1 for p in d if p['state']=='done')")"

v() { curl -sS "$API/verify/$1?engine=$2"; }

check "the safetensors model satisfies vllm" "True" \
  "$(v 'Qwen%2FQwen2.5-1.5B-Instruct' vllm | jqp "d['ok']")"
check "the GGUF model satisfies llama-cpp" "True" \
  "$(v 'bartowski%2FQwen2.5-0.5B-Instruct-GGUF' llama-cpp | jqp "d['ok']")"
# A safetensors repository has no config.json, so the old hardcoded check
# called it incomplete and an operator would re-download what was already fine.
check "the GGUF model is refused by vllm, with a reason" "True" \
  "$(v 'bartowski%2FQwen2.5-0.5B-Instruct-GGUF' vllm \
     | jqp "'error' in d and 'safetensors' in d['error']['message'] and 'gguf' in d['error']['message']")"
check "the safetensors model is refused by llama-cpp" "True" \
  "$(v 'Qwen%2FQwen2.5-1.5B-Instruct' llama-cpp | jqp "'error' in d")"
# An engine nobody has described must refuse rather than guess, because a
# wrong guess becomes a wrong readiness verdict.
check "an unprofiled engine refuses rather than guessing" "True" \
  "$(v 'Qwen%2FQwen2.5-1.5B-Instruct' tensorrt-llm | jqp "'error' in d")"

# ── operator reports ───────────────────────────────────────────────
section "operator inventory"

code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/operator/inventory" \
  -H 'Content-Type: application/json' -d '{
  "cluster":{"name":"k3s-dev","reachable":true,"version":"v1.36.4+k3s1",
    "cpuMillicores":32256,"memoryMiB":32768,
    "nodes":[
      {"name":"desktop-jkf1khq","ready":true,"roles":["control-plane","worker"],
       "gpu":{"model":"NVIDIA GeForce GT 720","count":1,"totalMemoryMiB":2048},
       "allocatableMemoryMiB":31800,"kubeletVersion":"v1.36.4","osImage":"Ubuntu 24.04"},
      {"name":"gpu-node-a","ready":true,"roles":["worker"],
       "gpu":{"model":"NVIDIA A100-SXM4-80GB","count":8,"totalMemoryMiB":655360},
       "allocatableMemoryMiB":980000,"kubeletVersion":"v1.36.4","osImage":"Ubuntu 24.04"}]},
  "deployments":[
    {"name":"qwen-7b","namespace":"fleet","model":"Qwen/Qwen2.5-7B-Instruct",
     "desiredReplicas":3,"readyReplicas":3,"tensorParallelSize":1,"pipelineParallelSize":0,
     "gpuPerReplica":1,"state":"Available"},
    {"name":"llama-70b","namespace":"fleet","model":"bartowski/Llama-3.3-70B-GGUF",
     "desiredReplicas":2,"readyReplicas":0,"tensorParallelSize":8,"pipelineParallelSize":2,
     "gpuPerReplica":16,"state":"Scheduling","reason":"InsufficientCapacity"}]
}')
check "POST /operator/inventory" "204" "$code"

C=$(curl -sS "$API/cluster")
# The counts are recomputed on report, so the summary can never disagree with
# the node list the operator actually sent.
check "the cluster summary counts the nodes" "2" "$(printf '%s' "$C" | jqp "d['clusters'][0]['nodeCount']")"
check "the cluster summary counts the GPUs" "9" "$(printf '%s' "$C" | jqp "d['clusters'][0]['gpuCount']")"
check "the cluster summary counts ready GPUs" "9" "$(printf '%s' "$C" | jqp "d['clusters'][0]['readyGpus']")"

D=$(curl -sS "$API/deployments")
# Pending:InsufficientCapacity and Pending:Scheduling are different states and
# a scheduler that cannot tell them apart either retries forever or gives up.
# Assert on the two just reported rather than on a count: this script is
# re-runnable, and earlier reports under other namespaces are still in the store.
check "the available deployment was recorded" "Available 3/3 TP=1 PP=0" \
  "$(printf '%s' "$D" | jqp "[f\"{x['state']} {x['readyReplicas']}/{x['desiredReplicas']} TP={x['tensorParallelSize']} PP={x['pipelineParallelSize']}\" for x in d if x['name']=='qwen-7b' and x['namespace']=='fleet'][0]")"
check "the capacity-starved deployment keeps its reason" "InsufficientCapacity" \
  "$(printf '%s' "$D" | jqp "[x['reason'] for x in d if x['name']=='llama-70b' and x['namespace']=='fleet'][0]")"
check "the capacity-starved deployment is Scheduling, not Pending" "Scheduling" \
  "$(printf '%s' "$D" | jqp "[x['state'] for x in d if x['name']=='llama-70b' and x['namespace']=='fleet'][0]")"

# ── authentication and rate limiting ───────────────────────────────
# A second gateway, on its own port, with authentication and a tight limit.
# The dev stack has both off — a developer's laptop must work with no setup —
# so the main GATEWAY above cannot be used to check this surface, and leaving
# it unchecked is how a gateway ships that serves anyone's requests.
section "gateway: authentication and rate limiting"

AUTH_PORT=${AUTH_PORT:-8099}
AUTH_GATEWAY=http://127.0.0.1:$AUTH_PORT
CHAT='{"model":"demo/Qwen2.5-1.5B-Instruct","messages":[{"role":"user","content":"hi"}]}'

# rpm=3 so the request refusal arrives inside a handful of requests, and tpm is
# left unset so the two dimensions are exercised separately below.
FLEET_AUTH_REQUIRED=true \
FLEET_API_KEYS='acme/admin|rpm=3;acme/second|rpm=99;other/third|rpm=99' \
  go -C core run ./cmd/fleet-gateway --listen "127.0.0.1:$AUTH_PORT" --demo \
  >"$WORKDIR/auth-gateway.log" 2>&1 &
AUTH_PID=$!

for _ in $(seq 1 100); do
  if curl -fsS "$AUTH_GATEWAY/healthz" >/dev/null 2>&1; then break; fi
  if ! kill -0 "$AUTH_PID" 2>/dev/null; then
    printf '%s the authenticated gateway exited during startup\n' "$(red 'ERROR')" >&2
    cat "$WORKDIR/auth-gateway.log" >&2
    exit 1
  fi
  sleep 0.2
done

if curl -fsS "$AUTH_GATEWAY/healthz" >/dev/null 2>&1; then
  # A probe cannot hold a credential, so a 401 here reports the pod as failing
  # while it is serving perfectly well.
  check "health needs no key" "200" \
    "$(curl -sS -o /dev/null -w '%{http_code}' "$AUTH_GATEWAY/healthz")"

  check "a valid key is served" "200" \
    "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' -H 'Authorization: Bearer admin' -d "$CHAT")"

  check "no key is refused" "401" \
    "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' -d "$CHAT")"

  check "an unknown key is refused" "401" \
    "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' -H 'Authorization: Bearer nope' -d "$CHAT")"

  # Headers are read with -D - rather than -I. curl's -I asks for HEAD, and
  # combining it with -X POST and -d makes curl print nothing at all — so the
  # check would read as a missing header on a server that is sending it.
  check "a 401 advertises how to authenticate" "True" \
    "$(curl -sS -D - -o /dev/null -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' -d "$CHAT" \
       | grep -qi 'www-authenticate: *bearer' && echo True || echo False)"

  # acme/admin is capped at 3 rpm and acme/second at 99. The tenant's bucket is
  # the tightest of its keys, so the admin key's ceiling is what applies and a
  # sibling key cannot be used to spend past it.
  for _ in 1 2 3; do
    curl -sS -o /dev/null -X POST "$AUTH_GATEWAY/v1/chat/completions" \
      -H 'Content-Type: application/json' -H 'Authorization: Bearer admin' -d "$CHAT"
  done
  check "the per-minute request limit refuses" "429" \
    "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' -H 'Authorization: Bearer admin' -d "$CHAT")"

  check "a refusal says when to retry" "True" \
    "$(curl -sS -D - -o /dev/null -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' -H 'Authorization: Bearer admin' -d "$CHAT" \
       | grep -qi '^retry-after: *[1-9]' && echo True || echo False)"

  check "a second key of the same tenant shares the bucket" "429" \
    "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' -H 'Authorization: Bearer second' -d "$CHAT")"

  # A different tenant has its own bucket. If it inherited acme's exhausted
  # allowance, a busy tenant would take the whole gateway down with it.
  check "another tenant is unaffected" "200" \
    "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' -H 'Authorization: Bearer third' -d "$CHAT")"
else
  printf '%s no authenticated gateway on :%s — see %s\n' \
    "$(red 'ERROR')" "$AUTH_PORT" "$WORKDIR/auth-gateway.log" >&2
  fail=$((fail + 1))
fi
kill "$AUTH_PID" 2>/dev/null || true
wait "$AUTH_PID" 2>/dev/null || true

# The token ceiling, on its own gateway so it is not spent by the checks above.
# tpm=200 with a request that reserves the prompt estimate plus the default
# max_tokens, so a couple of requests exhaust it. A limiter that only counted
# requests would serve all of these.
section "gateway: token limits"

TOKEN_PORT=${TOKEN_PORT:-8098}
TOKEN_GATEWAY=http://127.0.0.1:$TOKEN_PORT
FLEET_AUTH_REQUIRED=true \
FLEET_API_KEYS='acme/tight|tpm=200' \
FLEET_DEFAULT_MAX_TOKENS=100 \
  go -C core run ./cmd/fleet-gateway --listen "127.0.0.1:$TOKEN_PORT" --demo \
  >"$WORKDIR/token-gateway.log" 2>&1 &
TOKEN_PID=$!

for _ in $(seq 1 100); do
  if curl -fsS "$TOKEN_GATEWAY/healthz" >/dev/null 2>&1; then break; fi
  if ! kill -0 "$TOKEN_PID" 2>/dev/null; then
    printf '%s the token-limited gateway exited during startup\n' "$(red 'ERROR')" >&2
    cat "$WORKDIR/token-gateway.log" >&2
    exit 1
  fi
  sleep 0.2
done

if curl -fsS "$TOKEN_GATEWAY/healthz" >/dev/null 2>&1; then
  # Keep going until the ceiling is hit, so the check does not depend on the
  # exact prompt estimate: the first refusal is the assertion.
  code=200
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$TOKEN_GATEWAY/v1/chat/completions" \
      -H 'Content-Type: application/json' -H 'Authorization: Bearer tight' -d "$CHAT")
    [ "$code" = "429" ] && break
  done
  check "a token ceiling refuses before the request ceiling would" "429" "$code"

  # The other half — that the allowance comes back when the window rolls over —
  # is deliberately not checked here. Asserting it through the network costs a
  # 62-second sleep on every run, and a check nobody waits for is a check that
  # gets skipped. TestSettledUsageExpiresWithTheWindow asserts the same thing
  # with an injected clock, which is the reason the limiter takes one.
else
  printf '%s no token-limited gateway on :%s — see %s\n' \
    "$(red 'ERROR')" "$TOKEN_PORT" "$WORKDIR/token-gateway.log" >&2
  fail=$((fail + 1))
fi
kill "$TOKEN_PID" 2>/dev/null || true
wait "$TOKEN_PID" 2>/dev/null || true

# ── summary ────────────────────────────────────────────────────────
printf '\n'
if [ "$fail" -eq 0 ]; then
  printf '%s %d checks passed\n\n' "$(green 'PASS')" "$pass"
  exit 0
fi
printf '%s %d passed, %d failed\n\n' "$(red 'FAIL')" "$pass" "$fail"
exit 1

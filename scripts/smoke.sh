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

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
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

pq() {
  "$PY" "$HERE/exposition.py" "$1"
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

# ── embeddings ─────────────────────────────────────────────────────
section "gateway: embeddings"

EMB='{"model":"demo/Qwen2.5-1.5B-Instruct","input":"the quick brown fox"}'
E=$(curl -sS -X POST "$GATEWAY/v1/embeddings" -H 'Content-Type: application/json' -d "$EMB")
check "an embedding comes back" "True" \
  "$(printf '%s' "$E" | jqp "len(d['data'][0]['embedding']) > 0")"
check "the object is a list, not a completion" "list" \
  "$(printf '%s' "$E" | jqp "d['object']")"
# P6 again, on a second shape. Without usage the request is charged from what
# it reserved, and for an embedding the reservation is the input — close enough
# that only a direct comparison catches it.
check "embeddings report a nonzero usage" "True" \
  "$(printf '%s' "$E" | jqp "d['usage']['total_tokens'] > 0")"
check "an embedding spends no completion tokens" "0" \
  "$(printf '%s' "$E" | jqp "d['usage']['completion_tokens']")"

BATCH=$(curl -sS -X POST "$GATEWAY/v1/embeddings" -H 'Content-Type: application/json' \
  -d '{"model":"demo/Qwen2.5-1.5B-Instruct","input":[[1,2,3],[4,5],[6,7,8,9]]}')
check "a batch returns one vector per input" "3" \
  "$(printf '%s' "$BATCH" | jqp "len(d['data'])")"
# One usage for the whole batch, summing the ids. Per-item usage would make
# this 3; taking the largest item would make it 4. Only the sum is 9.
check "a batch is metered as the sum, not the largest item" "9" \
  "$(printf '%s' "$BATCH" | jqp "d['usage']['prompt_tokens']")"
check "indices are contiguous from zero" "True" \
  "$(printf '%s' "$BATCH" | jqp "[i['index'] for i in d['data']] == list(range(len(d['data'])))")"

# Token ids are counted from the ids: there is no text for a tokenizer to
# count, so a gateway that tokenized would report zero and reserve nothing.
check "token ids are counted, not ignored" "True" \
  "$(printf '%s' "$BATCH" | jqp "d['usage']['prompt_tokens'] > 0")"

# Every unusable input is refused. Accepting any of them would count zero
# tokens, reserve zero, and hand a caller a path through every limit.
for bad in '{"model":"m"}' \
           '{"input":"x"}' \
           '{"model":"m","input":[]}' \
           '{"model":"m","input":123}' \
           '{"model":"m","input":""}' \
           '{"model":"m","input":["a",[1,2]]}'
do
  check "refused: ${bad:0:34}" "400" \
    "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$GATEWAY/v1/embeddings" \
       -H 'Content-Type: application/json' -d "$bad")"
done

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

for route in models pulls storage engines clusters deployments; do
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

v() { curl -sS "$API/repositories/$1?engine=$2"; }

check "the safetensors model satisfies vllm" "True" \
  "$(v 'Qwen%2FQwen2.5-1.5B-Instruct' vllm | jqp "d['usable']")"
check "the GGUF model satisfies llama-cpp" "True" \
  "$(v 'bartowski%2FQwen2.5-0.5B-Instruct-GGUF' llama-cpp | jqp "d['usable']")"
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

code=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT "$API/inventory" \
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
check "PUT /inventory" "204" "$code"

# The contract is versioned and lives in another repository. These are the two
# ways that can silently break: a report from a newer sender, and a field this
# build does not recognise. Both must be tolerated — a receiver that rejected
# unknown fields would take a cluster's inventory down whenever the operator
# happened to be upgraded first. A separate cluster name so the counts asserted
# below still describe the report above.
code=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT "$API/inventory" \
  -H 'Content-Type: application/json' \
  -d '{"contract":99,"cluster":{"name":"from-the-future","reachable":true,"inventedByANewerOperator":[1,2,3]},
       "deployments":[{"name":"future","namespace":"fleet","model":"Qwen/Qwen2.5-7B-Instruct",
       "desiredReplicas":3,"readyReplicas":3,"state":"Available","address":"10.43.19.18:8000"}]}')
check "a newer operator reporting an unknown field is still accepted" "204" "$code"
check "and its deployment landed" "future" \
  "$(curl -sS "$API/deployments" | jqp "[d for d in d if d['name']=='future'][0]['name']")"

# A deployment with no identity has no key, so two of them collide and the
# second overwrites the first.
code=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT "$API/inventory" \
  -H 'Content-Type: application/json' \
  -d '{"cluster":{"name":"k3s-dev"},"deployments":[{"name":"","namespace":""}]}')
check "a deployment with no identity is refused" "400" "$code"

code=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT "$API/inventory" \
  -H 'Content-Type: application/json' -d '{"cluster":{}}')
check "a cluster with no name is refused" "400" "$code"

C=$(curl -sS "$API/clusters")
# The counts are recomputed on report, so the summary can never disagree with
# the node list the operator actually sent. Looked up by name rather than
# position: the section above deliberately adds a second cluster, and "[0]"
# would then describe whichever one sorted first.
check "the cluster summary counts the nodes" "2" \
  "$(printf '%s' "$C" | jqp "[c for c in d['clusters'] if c['name']=='k3s-dev'][0]['nodeCount']")"
check "the cluster summary counts the GPUs" "9" \
  "$(printf '%s' "$C" | jqp "[c for c in d['clusters'] if c['name']=='k3s-dev'][0]['gpuCount']")"
check "the cluster summary counts ready GPUs" "9" \
  "$(printf '%s' "$C" | jqp "[c for c in d['clusters'] if c['name']=='k3s-dev'][0]['readyGpus']")"

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

# acme has an envelope of 3 rpm and two projects of 3 rpm each. The point of
# that arithmetic is that it must NOT mean 6: if the two levels were merged
# into one figure the tenant would get the sum, and the envelope would be
# decorative. The checks below spend one project and then reach for the other.
FLEET_AUTH_REQUIRED=true \
FLEET_API_KEYS='acme/research/admin;acme/batch/second;other/third/third;embed/one/embed' \
FLEET_RATE_TENANTS='acme|rpm=3' \
FLEET_RATE_PROJECTS='acme/research|rpm=3;acme/batch|rpm=3' \
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
       -H 'Content-Type: application/json' \
       -H 'Authorization: Bearer acme/research/admin' -d "$CHAT")"

  check "no key is refused" "401" \
    "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' -d "$CHAT")"

  # The rolling log is what the console answers "who spent this" from. Without
  # the scope on it, that panel is a performance graph and an operator with a
  # bill to allocate has nowhere to look. The two halves are separate fields
  # so the console can group by either; the id is their composition.
  check "a served request says which project spent it" "acme/research" \
    "$(curl -sS "$AUTH_GATEWAY/fleet/status" \
       -H 'Authorization: Bearer acme/research/admin' \
       | jqp "next('/'.join([r['tenant'], r['project']]) for r in d['recent'] if r.get('project'))")"

  check "an unknown key is refused" "401" \
    "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' -H 'Authorization: Bearer nope' -d "$CHAT")"

  # Two tenants may both name a key "third". A store keyed on the bare key id
  # would let one tenant's credential resolve to another tenant's principal,
  # which is a cross-tenant read of somebody else's ledger.
  check "a bare key id belongs to nobody" "401" \
    "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' -H 'Authorization: Bearer third' -d "$CHAT")"

  # Headers are read with -D - rather than -I. curl's -I asks for HEAD, and
  # combining it with -X POST and -d makes curl print nothing at all — so the
  # check would read as a missing header on a server that is sending it.
  check "a 401 advertises how to authenticate" "True" \
    "$(curl -sS -D - -o /dev/null -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' -d "$CHAT" \
       | grep -qi 'www-authenticate: *bearer' && echo True || echo False)"

  # acme/research is capped at 3 rpm. Spend it.
  for _ in 1 2 3; do
    curl -sS -o /dev/null -X POST "$AUTH_GATEWAY/v1/chat/completions" \
      -H 'Content-Type: application/json' \
      -H 'Authorization: Bearer acme/research/admin' -d "$CHAT"
  done
  check "the per-minute request limit refuses" "429" \
    "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' \
       -H 'Authorization: Bearer acme/research/admin' -d "$CHAT")"

  check "a refusal says when to retry" "True" \
    "$(curl -sS -D - -o /dev/null -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' \
       -H 'Authorization: Bearer acme/research/admin' -d "$CHAT" \
       | grep -qi '^retry-after: *[1-9]' && echo True || echo False)"

  # The refusal has to say WHICH budget ran out, or a caller goes looking in the
  # wrong one.
  check "a refusal names the scope that ran out" "True" \
    "$(curl -sS -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' \
       -H 'Authorization: Bearer acme/research/admin' -d "$CHAT" \
       | grep -q 'acme/research' && echo True || echo False)"

  # The tenant's second project is still empty — 3 rpm each, and only research
  # has spent any. It must still be refused, because the tenant envelope is at
  # 3. If it answers 200 the two limits were added rather than nested.
  check "a second project cannot spend past the envelope" "429" \
    "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' \
       -H 'Authorization: Bearer acme/batch/second' -d "$CHAT")"

  # A different tenant has its own envelope. If it inherited acme's exhausted
  # allowance, a busy tenant would take the whole gateway down with it.
  check "another tenant is unaffected" "200" \
    "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AUTH_GATEWAY/v1/chat/completions" \
       -H 'Content-Type: application/json' \
       -H 'Authorization: Bearer other/third/third' -d "$CHAT")"
  # -- what Fleet publishes about itself ------------------------------
  #
  # Inside the authenticated section rather than after it: that gateway is
  # killed the moment its own checks finish, and it is the only one of the
  # three with a tenant to attribute spending to. With authentication off there
  # is no tenant, and a series carrying an empty tenant label answers none of
  # the questions these exist for.
  section "gateway: metrics"

  # other/third, not acme/research: the checks above deliberately exhaust
  # acme's three-per-minute envelope, so a request under acme here would be
  # refused with a 429 and the assertion would be measuring the limiter.
  MK='other/third/third'
  # Not exempt from authentication, unlike /healthz. The exposition carries
  # per-tenant token counts and spend, which is billing data; a probe needs no
  # credential, a scraper carries one.
  check "metrics need a credential" "401" \
    "$(curl -sS -o /dev/null -w '%{http_code}' "$AUTH_GATEWAY/metrics")"
  check "a scraper with a key gets the exposition" "200" \
    "$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $MK" "$AUTH_GATEWAY/metrics")"
  # Headers off a GET, not -I: chi routes GET and does not answer HEAD, so a
  # HEAD here is a 405 with no headers and proves nothing at all.
  check "it is the text format, versioned" "True" \
    "$(curl -sS -D - -o /dev/null -H "Authorization: Bearer $MK" "$AUTH_GATEWAY/metrics" \
       | grep -i '^content-type:' | grep -qi 'version=0.0.4' && echo True || echo False)"

  curl -sS -o /dev/null -X POST "$AUTH_GATEWAY/v1/chat/completions" \
    -H 'Content-Type: application/json' -H "Authorization: Bearer $MK" \
    -d '{"model":"demo/Qwen2.5-1.5B-Instruct","messages":[{"role":"user","content":"hi"}],"max_tokens":16}'

  # A tenant of its own, because the completion assertion is about the
  # embedding alone, and sharing a tenant with the chat request above would
  # make it untrue of the combined series.
  EK='embed/one/embed'
  curl -sS -o /dev/null -X POST "$AUTH_GATEWAY/v1/embeddings" \
    -H 'Content-Type: application/json' -H "Authorization: Bearer $EK" \
    -d '{"model":"demo/Qwen2.5-1.5B-Instruct","input":"a document"}'

  M=$(curl -sS -H "Authorization: Bearer $MK" "$AUTH_GATEWAY/metrics")
  check "a served request is counted" "True" \
    "$(printf '%s' "$M" | pq "any(s['name']=='fleet_requests_total' and s['value']>0 for s in samples)")"
  # The tenant the key resolved to, and the bare project name rather than the
  # qualified id the ledger row stores.
  check "and it names the tenant that spent it" "True" \
    "$(printf '%s' "$M" | pq "any(s['name']=='fleet_requests_total' and s['labels'].get('tenant')=='other' and s['labels'].get('project')=='third' for s in samples)")"
  # Split by which side of the bill they are on: fresh, cached and completion
  # are billed at three different rates and a single sum destroys that.
  check "tokens are split by billing side" "True" \
    "$(printf '%s' "$M" | pq "any(s['name']=='fleet_tokens_total' and s['labels'].get('kind')=='fresh' for s in samples)")"
  check "cached tokens have their own series" "True" \
    "$(printf '%s' "$M" | pq "any(s['name']=='fleet_tokens_total' and s['labels'].get('kind')=='cached' for s in samples)")"
  # Every kind gets a series on every request, including at zero, so this is a
  # statement about the value: an embedding has prompt tokens and none at all
  # on the completion side.
  check "an embedding spends no completion tokens" "True" \
    "$(printf '%s' "$M" | pq "any(s['name']=='fleet_tokens_total' and s['labels'].get('kind')=='prompt' and s['labels'].get('tenant')=='embed' and s['value']>0 for s in samples) and any(s['name']=='fleet_tokens_total' and s['labels'].get('kind')=='completion' and s['labels'].get('tenant')=='embed' and s['value']==0 for s in samples)")"
  check "latency is a histogram ending at +Inf" "True" \
    "$(printf '%s' "$M" | pq "any(s['name']=='fleet_request_duration_seconds_bucket' and s['labels'].get('le')=='+Inf' for s in samples)")"
  # Cumulative means monotone: +Inf is the total and no narrower bucket may
  # exceed it. Per-bucket counts would decrease, which every server accepts and
  # every dashboard then renders as a shrinking distribution.
  check "and the buckets never decrease" "True" \
    "$(printf '%s' "$M" | pq "(all(b[i] <= b[i+1] for i in range(len(b)-1)) and len(b)>0) if (b := [x['value'] for x in sorted([s for s in samples if s['name']=='fleet_request_duration_seconds_bucket' and s['labels'].get('tenant')=='other'], key=lambda x: float(x['labels']['le'].replace('+Inf','1e99')))]) else False")"
  check "endpoints are counted by health" "True" \
    "$(printf '%s' "$M" | pq "any(s['name']=='fleet_endpoints' for s in samples)")"
  check "the build identifies itself" "True" \
    "$(printf '%s' "$M" | pq "any(s['name']=='fleet_build_info' and s['labels'].get('edition')=='community' for s in samples)")"
  # Keys rotate; a key label is a series that appears and vanishes forever.
  check "no series is labelled with a key" "False" \
    "$(printf '%s' "$M" | pq "any('key' in s['labels'] for s in samples)")"

  # A refused request has to be visible, or a fleet that began rejecting keys
  # looks exactly like a fleet with no traffic.
  curl -sS -o /dev/null -X POST "$AUTH_GATEWAY/v1/chat/completions" \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer sk-fleet-not-real' \
    -d '{"model":"demo/Qwen2.5-1.5B-Instruct","messages":[]}'
  check "and a refused request shows up as a refusal" "True" \
    "$(curl -sS -H "Authorization: Bearer $MK" "$AUTH_GATEWAY/metrics" \
       | pq "any(s['name']=='fleet_refused_total' and s['value']>0 for s in samples)")"


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
FLEET_API_KEYS='acme/research/tight' \
FLEET_RATE_PROJECTS='acme/research|tpm=200' \
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
      -H 'Content-Type: application/json' -H 'Authorization: Bearer acme/research/tight' -d "$CHAT")
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

# ── tenancy, keys and budgets ──────────────────────────────────────
# Only with a database: without one the control plane is deliberately still
# running, and a check that skips itself is worse than no check. Run with
# FLEET_DATABASE_URL pointing at a scratch database to exercise this.
if curl -sS "$API/tenants" | grep -q '"code"'; then
  section "tenancy (no database; skipped)"
else
  section "tenancy, keys and budgets"

  T="smoke-$$"
  code=$(curl -sS -o "$WORKDIR/t.json" -w '%{http_code}' -X POST "$API/tenants" \
    -H 'content-type: application/json' \
    -d "{\"id\":\"$T\",\"name\":\"Smoke\",\"requestLimit\":1000,\"tokenLimit\":200000}")
  check "POST /tenants" "201" "$code"

  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/projects" \
    -H 'content-type: application/json' \
    -d "{\"tenantId\":\"$T\",\"name\":\"research\",\"requestLimit\":500,\"tokenLimit\":100000}")
  check "POST /projects" "201" "$code"

  check "GET /tenants/* includes the project" "$T/research" \
    "$(curl -sS "$API/tenants/$T" | jqp "d['projects'][0]['id']")"

  # A project above its tenant's envelope is a partition that would give the
  # tenant more capacity than the envelope, so it is refused on write.
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/projects" \
    -H 'content-type: application/json' \
    -d "{\"tenantId\":\"$T\",\"name\":\"greedy\",\"requestLimit\":9999}")
  check "a project above the envelope is refused" "400" "$code"

  K=$(curl -sS -X POST "$API/keys" -H 'content-type: application/json' \
    -d "{\"projectId\":\"$T/research\",\"label\":\"smoke\"}")
  check "the key secret is returned once" "True" \
    "$(printf '%s' "$K" | jqp "d['secret'].startswith('sk-fleet-')")"
  KID=$(printf '%s' "$K" | jqp "d['id']")

  # Only a hash is stored, so a second read cannot return the secret. If it
  # ever can, the key is readable from the database by anyone with a copy.
  check "the key list never carries the secret" "False" \
    "$(curl -sS "$API/keys?project=$T%2Fresearch" | jqp "'secret' in d[0]")"

  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/budget-rules" \
    -H 'content-type: application/json' \
    -d "{\"scopeKind\":\"tenant\",\"scopeId\":\"$T\",\"dimension\":\"tokens_total\",\"limit\":5000,\"window\":\"5h\"}")
  check "POST /budget-rules" "204" "$code"

  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/budget-rules" \
    -H 'content-type: application/json' \
    -d "{\"scopeKind\":\"tenant\",\"scopeId\":\"$T\",\"dimension\":\"units\",\"limit\":10,\"window\":\"1mo\"}")
  check "a second dimension over a second window is accepted" "204" "$code"

  check "both rules are listed" "2" \
    "$(curl -sS "$API/budget-rules?scopeId=$T" | jqp "len(d)")"

  # A rule cannot be applied to a dimension that does not exist, and guessing
  # one would silently bill against a counter nothing writes.
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/budget-rules" \
    -H 'content-type: application/json' \
    -d "{\"scopeKind\":\"tenant\",\"scopeId\":\"$T\",\"dimension\":\"tokens_vibes\",\"limit\":1,\"window\":\"1h\"}")
  check "an unknown dimension is refused" "400" "$code"

  # "month" is 30 days, not a calendar month: a rolling window that is
  # sometimes 28 days is not a budget a tenant can reason about.
  check "a one-month window is 30 days" "2592000" \
    "$(curl -sS "$API/budget-rules?scopeId=$T&scopeKind=tenant" | jqp "[r['windowSeconds'] for r in d if r['dimension']=='units'][0]")"

  code=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE \
     "$API/budget-rules/tenant/$T/tokens_total/5h")
  check "DELETE /budget-rules/* removes only that window" "204" "$code"
  check "the other rule survived" "1" \
    "$(curl -sS "$API/budget-rules?scopeId=$T" | jqp "len(d)")"

  # A project scopeId is itself "tenant/name", so the delete id has five
  # segments rather than four. Splitting it positionally made every project
  # rule undeletable — the one operation an operator needs when a cap is wrong.
  curl -sS -o /dev/null -X POST "$API/budget-rules" \
    -H 'content-type: application/json' \
    -d "{\"scopeKind\":\"project\",\"scopeId\":\"$T/research\",\"dimension\":\"units\",\"limit\":5,\"window\":\"1h\"}"
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE \
     "$API/budget-rules/project/$T/research/units/1h")
  check "a project rule is deletable despite the slash in its id" "204" "$code"
  check "and it is really gone" "0" \
    "$(curl -sS "$API/budget-rules?scopeId=$T/research" | jqp "len(d)")"

  code=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$API/keys/$KID")
  check "DELETE /keys/*" "204" "$code"
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$API/projects/$T/research")
  check "DELETE /projects/*" "204" "$code"
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$API/tenants/$T")
  check "DELETE /tenants/*" "204" "$code"
  check "the tenant is gone" "404" \
    "$(curl -sS -o /dev/null -w '%{http_code}' "$API/tenants/$T")"
fi

# ── the cost pool ──────────────────────────────────────────────────
# P8: what the fleet cost, what of it was used, and who is billed for it. Fleet
# only started watching this cluster, so a month nobody reported on cannot be
# closed — and refusing is the behaviour under test, not an obstacle to it.
if curl -sS "$API/cost-rates" | grep -q '"code"'; then
  section "cost pool (no database; skipped)"
else
  section "cost pool"

  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/cost-rates" \
    -H 'content-type: application/json' -d '{"cluster":"","gpuHourMicro":1}')
  check "a cost rate needs a cluster" "400" "$code"
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/cost-rates" \
    -H 'content-type: application/json' -d '{"cluster":"c1","gpuHourMicro":-5}')
  check "a cost rate cannot be negative" "400" "$code"

  curl -sS -o /dev/null -X POST "$API/cost-rates" -H 'content-type: application/json' \
    -d '{"cluster":"smoke-cluster","gpuHourMicro":3000000,"currency":"USD"}'
  # Selected by name, not by position. A seeded rate that sorts earlier lands
  # at index 0, and the check then reads somebody else's number and calls it a
  # failure — or, worse, passes against the wrong row.
  check "a declared rate reads back" "3000000" \
    "$(curl -sS "$API/cost-rates" | jqp "[r['gpuHourMicro'] for r in d if r['cluster']=='smoke-cluster'][0]")"

  code=$(curl -sS -o /dev/null -w '%{http_code}' "$API/cost-periods/January")
  check "a period is YYYY-MM" "400" "$code"
  code=$(curl -sS -o /dev/null -w '%{http_code}' "$API/cost-periods/1999-02")
  check "a period nobody closed is a 404" "404" "$code"

  # A month Fleet has not been watching. The close must refuse: a pool of zero
  # is indistinguishable from a fleet that cost nothing.
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT "$API/cost-periods/1999-02")
  check "an unwatched period will not close" "400" "$code"
  msg=$(curl -sS -X PUT "$API/cost-periods/1999-02" | jqp "d['error']['message']")
  check "and it says how much of it it saw" "yes" \
    "$(case "$msg" in *observed*) echo yes;; *) echo "no: $msg";; esac)"

  # Forcing the coverage gate closes a period for real, so this run may be the
  # first to do it (201) or may be recomputing one a previous run closed (200).
  # 409 is not reachable here: nothing after 1999-01 has been closed, so there
  # is no month for a correction to land in and nothing to freeze.
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT "$API/cost-periods/1999-01?minCoverage=0")
  case "$code" in
    201|200) ;;
    *) check "a forced close creates or recomputes the period" "201 or 200" "$code" ;;
  esac

  closed=$(curl -sS "$API/cost-periods/1999-01")
  check "the closed period reads back" "1999-01" "$(printf '%s' "$closed" | jqp "d['period']")"
  check "closed with no capacity, so unpriced" "False" "$(printf '%s' "$closed" | jqp "d['priced']")"
  check "and it recorded no coverage rather than claiming all of it" "0" \
    "$(printf '%s' "$closed" | jqp "d['coveragePercent']")"
  check "an unpriced pool is zero, not missing" "0" "$(printf '%s' "$closed" | jqp "d['pool']")"
  check "and it appears in the list" "True" \
    "$(curl -sS "$API/cost-periods" | jqp "any(p['period']=='1999-01' for p in d)")"

  # Closing it again recomputes rather than conflicting. 1999-02 was refused
  # above, so nothing later exists that could freeze it.
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT "$API/cost-periods/1999-01?minCoverage=0")
  check "closing it again is a revision, not a conflict" "200" "$code"
  check "the period says how many times it has been computed" "True" \
    "$(curl -sS "$API/cost-periods/1999-01" | jqp "d['revision']>=1")"
  check "a revision carries no correction into itself" "0" \
    "$(curl -sS "$API/cost-periods/1999-01" | jqp "d['adjustmentTotal']")"
fi

# ── the metering audit ─────────────────────────────────────────────
# Fleet's own two measurements of output tokens, side by side. The point is
# not the number this run produces — a fresh ledger has one population and no
# comparison to make — but that an empty comparison is an empty table and not a
# null, and that the policy in force is echoed so a finding is reproducible.
if curl -sS "$API/cost-rates" | grep -q '"code"'; then
  section "metering agreement (no database; skipped)"
else
  section "metering agreement"

  ag=$(curl -sS "$API/usage-agreement")
  check "the audit answers" "True" "$(printf '%s' "$ag" | jqp "d['period'] is not None")"
  check "an empty source mix is an object, not null" "True" \
    "$(printf '%s' "$ag" | jqp "isinstance(d['sources'], dict)")"
  check "keys are a list rather than null" "True" \
    "$(printf '%s' "$ag" | jqp "isinstance(d['keys'], list)")"
  # Both thresholds are echoed, so the same rows under a different policy can be
  # told apart from the same rows under this one.
  check "the thresholds in force are reported" "True" \
    "$(printf '%s' "$ag" | jqp "d['minSamples']>=1 and d['tolerancePercent']>=1")"
  # Every key carries a reason, so "no findings" is distinguishable from "the
  # check declined to look".
  check "every endpoint says why it passed or failed" "True" \
    "$(printf '%s' "$ag" | jqp "all(k.get('reason') for k in d['keys'])")"
  check "the fault count matches the table" "True" \
    "$(printf '%s' "$ag" | jqp "d['faults']==sum(1 for k in d['keys'] if k['fault'])")"
fi

# ── summary ────────────────────────────────────────────────────────
printf '\n'
if [ "$fail" -eq 0 ]; then
  printf '%s %d checks passed\n\n' "$(green 'PASS')" "$pass"
  exit 0
fi
printf '%s %d passed, %d failed\n\n' "$(red 'FAIL')" "$pass" "$fail"
exit 1

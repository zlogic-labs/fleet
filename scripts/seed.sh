#!/usr/bin/env bash
# Seed the dev control plane with data, so every console page has something in it.
#
#   ./scripts/seed.sh
#
# Safe to re-run: it only adds registry entries and pull history. Nothing here
# downloads a real model, and the Cluster page is filled by a hand-written
# inventory report, which is exactly the shape the operator will POST.

set -euo pipefail
BASE=${CONTROL:-http://127.0.0.1:8081}
GATEWAY=${GATEWAY:-http://127.0.0.1:8080}
API=$BASE/api/v1

say() { printf '%-46s %s\n' "$1" "$2"; }

# An RFC3339 timestamp N hours into September 2026, for replaying a month of
# capacity reports. Uses the interpreter already resolved below rather than
# `date -d`, because that is GNU-specific and the same script has to run on a
# macOS laptop.
month_ts() {
  "$PY" -c "
import datetime,sys
base=datetime.datetime(2026,9,1,tzinfo=datetime.timezone.utc)
print((base+datetime.timedelta(hours=int(sys.argv[1]))).strftime('%Y-%m-%dT%H:%M:%SZ'))" "$1"
}

# Resolved once, up front. Ubuntu ships python3 with no "python" shim, and
# assuming the name turns every summary below into an empty section that reads
# as "no models yet" rather than as a broken script.
#
# Each candidate is executed, not merely located. `command -v` is not enough:
# Windows installs an App Execution Alias named python3 that resolves on PATH
# and then fails when run, so a check-then-run split here passes the detection
# and fails every assertion afterwards. Finding the interpreter is only evidence
# if the interpreter answers.
PY=""
for candidate in python3 python; do
  if command -v "$candidate" >/dev/null 2>&1 && "$candidate" -c "" >/dev/null 2>&1; then
    PY=$candidate
    break
  fi
done
if [ -z "$PY" ]; then
  echo "no working python3 or python on PATH; cannot print the summary" >&2
  exit 1
fi

if ! curl -fsS "$BASE/healthz" >/dev/null 2>&1; then
  echo "no control plane at $BASE — start ./scripts/dev.sh first" >&2
  exit 1
fi

pull() {
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/pulls" \
    -H 'Content-Type: application/json' -d "$1")
  say "pull ${2}" "$code"
}

# Two that succeed, so the Registry card has ready rows.
# No tokenizerId on purpose. The field is a tiktoken encoding name, not a model
# name: the gateway's resolver returns a non-empty hint verbatim and skips its
# own prefix matching, so a guess here suppresses the exact table. Leave it
# empty and the resolver picks by model name, which is correct.
pull '{"model":"Qwen/Qwen2.5-1.5B-Instruct","source":"huggingface","sourceRef":"Qwen/Qwen2.5-1.5B-Instruct","engine":"vllm","contextLimit":32768}' \
  'Qwen/Qwen2.5-1.5B-Instruct (safetensors, for vLLM)'
# A GGUF repository, so the two weight formats sit side by side. It is the case
# that a hardcoded config.json check got wrong: the repository has no
# config.json, only .gguf files, and llama.cpp reads it while vLLM cannot.
pull '{"model":"bartowski/Qwen2.5-0.5B-Instruct-GGUF","source":"huggingface","sourceRef":"bartowski/Qwen2.5-0.5B-Instruct-GGUF","engine":"llama-cpp"}' \
  'bartowski/Qwen2.5-0.5B-Instruct-GGUF (gguf, for llama.cpp)'
# One that fails, so the failure path is visible in the console.
pull '{"model":"fleet/broken-model"}' 'fleet/broken-model (fails on purpose)'

echo "waiting for pulls to settle"
for _ in $(seq 1 60); do
  running=$(curl -sS "$API/pulls" | grep -c '"state":"\(queued\|running\)"' || true)
  [ "$running" = "0" ] && break
  sleep 0.25
done

# A cluster report, the way the operator will send it. The GT 720 is this
# machine's actual GPU and it is not enough for vLLM, which is the point: the
# Cluster page should show why a real deployment cannot start here.
#
# Units, because they are easy to get wrong by a factor of 1024 and the console
# just renders whatever it is given:
#   cpuMillicores       32 cores      -> 32256
#   memoryMiB           32 GiB        -> 32768
#   gpu.totalMemoryMiB  8x 80 GiB     -> 655360
#   allocatableMemoryMiB  31 GiB      -> 31800
code=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT "$API/inventory" \
  -H 'Content-Type: application/json' -d '{
  "cluster": {
    "name": "k3s-dev", "reachable": true, "version": "v1.36.4+k3s1",
    "cpuMillicores": 32256, "memoryMiB": 32768,
    "nodes": [
      {"name":"desktop-jkf1khq","ready":true,"roles":["control-plane","worker"],
       "gpu":{"model":"NVIDIA GeForce GT 720","count":1,"totalMemoryMiB":2048},
       "allocatableMemoryMiB":31800,"kubeletVersion":"v1.36.4","osImage":"Ubuntu 24.04",
       "addresses":{"internal":"172.31.74.144"}},
      {"name":"gpu-node-a","ready":true,"roles":["worker"],
       "gpu":{"model":"NVIDIA A100-SXM4-80GB","count":8,"totalMemoryMiB":655360},
       "allocatableMemoryMiB":980000,"kubeletVersion":"v1.36.4","osImage":"Ubuntu 24.04",
       "addresses":{"internal":"10.0.1.11"}}
    ]
  },
  "deployments": [
    {"name":"qwen-7b","namespace":"fleet","model":"Qwen/Qwen2.5-7B-Instruct",
     "desiredReplicas":3,"readyReplicas":3,"tensorParallelSize":1,"pipelineParallelSize":0,
     "gpuPerReplica":1,"state":"Available"},
    {"name":"llama-70b","namespace":"fleet","model":"meta-llama/Llama-3.3-70B-Instruct",
     "desiredReplicas":2,"readyReplicas":0,"tensorParallelSize":8,"pipelineParallelSize":2,
     "gpuPerReplica":16,"state":"Pending",
     "reason":"InsufficientCapacity: 1 of 16 GPUs schedulable"}
  ]
}')
say "operator inventory report" "$code"

# Traffic, so the Fleet page's recent-requests table is not empty.
for i in 1 2 3 4 5 6; do
  curl -sS -o /dev/null -X POST "$GATEWAY/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"demo/Qwen2.5-1.5B-Instruct","messages":[{"role":"user","content":"What is prefix caching, and why does it matter for a gateway?"}],"max_tokens":180}'
done
say "6 requests through the gateway" ok

# Tenancy, so the Tenancy page is not an empty shell. Tenants exist to be
# budgeted, so each one gets both levels: an envelope and a project slice of it.
ten() { curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/$1" -H 'Content-Type: application/json' -d "$2"; }

for body in \
  '{"id":"acme","name":"Acme Corp","requestLimit":600,"tokenLimit":4000000}' \
  '{"id":"globex","name":"Globex","requestLimit":300,"tokenLimit":1500000}' \
  '{"id":"initech","name":"Initech","requestLimit":60,"tokenLimit":200000}'
do
  say "tenant $(printf '%s' "$body" | sed 's/.*"id":"\([^"]*\)".*/\1/')" "$(ten tenants "$body")"
done

for body in \
  '{"tenantId":"acme","name":"research"}' \
  '{"tenantId":"acme","name":"batch"}' \
  '{"tenantId":"globex","name":"chat"}' \
  '{"tenantId":"initech","name":"eval"}'
do
  say "project $(printf '%s' "$body" | sed 's/.*"name":"\([^"]*\)".*/\1/')" "$(ten projects "$body")"
done

# Two dimensions on two windows for one scope, because the point of the budget
# model is that they are independent: a monthly money cap and a five-hour token
# cap do not constrain each other, and removing one leaves the other standing.
budget() {
  curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/budget-rules" \
    -H 'Content-Type: application/json' -d "$1"
}
say "budget acme units/1mo" \
  "$(budget '{"scopeKind":"tenant","scopeId":"acme","dimension":"units","limit":500000,"window":"1mo"}')"
say "budget acme tokens_total/5h" \
  "$(budget '{"scopeKind":"tenant","scopeId":"acme","dimension":"tokens_total","limit":2000000,"window":"5h"}')"
say "budget acme/research units/1mo" \
  "$(budget '{"scopeKind":"project","scopeId":"acme/research","dimension":"units","limit":300000,"window":"1mo"}')"
say "budget acme/research tokens_fresh/1d" \
  "$(budget '{"scopeKind":"project","scopeId":"acme/research","dimension":"tokens_fresh","limit":400000,"window":"1d"}')"
say "budget globex/chat units/1mo" \
  "$(budget '{"scopeKind":"project","scopeId":"globex/chat","dimension":"units","limit":120000,"window":"1mo"}')"

code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/keys" \
  -H 'Content-Type: application/json' -d '{"projectId":"acme/research","label":"ci"}')
say "key acme/research/ci" "$code"
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/keys" \
  -H 'Content-Type: application/json' -d '{"projectId":"globex/chat","label":"app"}')
say "key globex/chat/app" "$code"

# The cost pool. A rate is declared rather than inferred, so without this the
# Cost page has nothing but the "unpriced" warning to show.
say "cost rate k3s-dev" \
  "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/cost-rates" \
     -H 'Content-Type: application/json' \
     -d '{"cluster":"k3s-dev","gpuHourMicro":2000000,"currency":"USD"}')"

# Capacity is a time series, so one report is not a month. A report with no
# reportedAt is refused outright — Fleet will not guess when a GPU was present
# — which means a single inventory POST produces a period with 0% coverage and
# a pool of nothing. The operator really does report every five minutes; this
# replays that shape at six-hour spacing across one month, which integrates to
# the same total.
#
# The fleet is left idle on purpose. A pool that was paid for and never used is
# the report this platform exists to produce, so the seeded month says exactly
# that: 8 GPUs, zero requests, 100% idle.
inventory() {
  curl -sS -o /dev/null -w '%{http_code}' -X PUT "$API/inventory" \
    -H 'Content-Type: application/json' -d "$1"
}
say "capacity reports across 2026-09" "$(for h in $(seq 0 6 714); do
  at=$(month_ts $h)
  inventory "{
    \"contract\":1,
    \"cluster\": {\"name\":\"k3s-dev\",\"reachable\":true,\"version\":\"v1.36.4+k3s1\",
      \"cpuMillicores\":32256,\"memoryMiB\":32768,
      \"nodes\":[{\"name\":\"gpu-node-a\",\"ready\":true,\"roles\":[\"worker\"],
        \"gpu\":{\"model\":\"NVIDIA A100-SXM4-80GB\",\"count\":8,\"totalMemoryMiB\":655360},
        \"allocatableMemoryMiB\":980000,\"kubeletVersion\":\"v1.36.4\",
        \"osImage\":\"Ubuntu 24.04\",\"addresses\":{\"internal\":\"10.0.1.11\"}}],
      \"reportedAt\":\"$at\"},
    \"deployments\":[{\"name\":\"qwen-7b\",\"namespace\":\"fleet\",\"model\":\"Qwen/Qwen2.5-7B-Instruct\",
      \"desiredReplicas\":3,\"readyReplicas\":3,\"tensorParallelSize\":1,\"pipelineParallelSize\":0,
      \"gpuPerReplica\":1,\"state\":\"Available\",\"reportedAt\":\"$at\"}]
  }"
done | tr '\n' ' ')"

# Now close the month. minCoverage is left at its default on purpose: the
# reports above really do span the period, so if the gate refuses, the gate is
# telling the truth about a month it did not observe.
for period in 2026-09; do
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT "$API/cost-periods/$period")
  say "close $period" "$code"
done

echo
echo "state:"
# No error masking on these: a summary that prints nothing must not look the
# same as a registry that is genuinely empty, which is how a broken script
# reads as an empty system.
curl -sS "$API/models" | "$PY" -c "
import json,sys
for m in json.load(sys.stdin):
    # The tokenizer is empty for GGUF by design: it is inside the weights file.
    tok = m['tokenizerId'] or ('embedded' if m['format']=='gguf' else '—')
    print(f\"  {m['name']:38} {m['format']:12} {m['state']:8} {m['files']:>3} files  tokenizer={tok}\")"
curl -sS "$API/engines" | "$PY" -c "
import json,sys
for e in json.load(sys.stdin):
    print(f\"  engine {e['name']:10} loads={e['format']:12} minCompute={e['minCompute']:<4} metrics={str(e['metrics']):5} capacity={str(e['capacity']):5} tokenize={e['tokenize']}\")"
curl -sS "$API/storage" | "$PY" -c "
import json,sys
s=json.load(sys.stdin)
print(f\"  storage: {s['reachable'] and 'reachable' or 'UNREACHABLE'}  {s['objectCount']} objects  {s['usedBytes']} bytes\")"
curl -sS "$API/tenants" | "$PY" -c "
import json,sys,urllib.request
ts=json.load(sys.stdin)
print(f\"  {len(ts)} tenants\")
for t in ts:
    # The list carries no projects; the detail endpoint does. Counting from the
    # list would print 0 for a tenant that has three.
    d=json.load(urllib.request.urlopen('$API/tenants/'+t['id']))
    print(f\"    {t['id']:10} rpm={t['requestLimit']:<7} tpm={t['tokenLimit']:<9} projects={len(d.get('projects') or [])}\")"
curl -sS "$API/cost-periods" | "$PY" -c "
import json,sys
ps=json.load(sys.stdin)
print(f\"  {len(ps)} closed periods\")
for p in ps:
    # Revision is printed because a second run of this script recomputes the
    # month rather than failing on it, and a number that quietly goes to 2 is
    # the one a reader would want to see.
    rev='' if p.get('revision', 1) <= 1 else f\"  rev{p['revision']}\"
    print(f\"    {p['period']}  pool={p['pool']}  idle={p['idlePercent']}%  coverage={p['coveragePercent']}%{rev}\")"

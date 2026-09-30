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
pull '{"model":"Qwen/Qwen2.5-1.5B-Instruct","contextLimit":32768,"tokenizerId":"qwen2"}' \
  'Qwen/Qwen2.5-1.5B-Instruct'
pull '{"model":"Qwen/Qwen2.5-7B-Instruct","contextLimit":32768,"tokenizerId":"qwen2"}' \
  'Qwen/Qwen2.5-7B-Instruct'
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
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/operator/inventory" \
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

echo
echo "state:"
curl -sS "$API/models" | python -c "
import json,sys
for m in json.load(sys.stdin):
    print(f\"  {m['name']:32} {m['state']:8} {m['files']:>3} files  ctx={m['contextLimit']}\")" 2>/dev/null || true
curl -sS "$API/storage" | python -c "
import json,sys
s=json.load(sys.stdin)
print(f\"  storage: {s['reachable'] and 'reachable' or 'UNREACHABLE'}  {s['objectCount']} objects  {s['usedBytes']} bytes\")" 2>/dev/null || true

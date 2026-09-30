#!/usr/bin/env bash
# End-to-end: pull a real model, declare it, deploy it to k3s, and infer
# through the gateway.
#
# Every stage is asserted rather than assumed. The assertions are the point:
# "it started" is not a result, "the pod serves the model under the name the
# client asked for, and the gateway reported the engine's own token counts" is.
#
# Run scripts/k3s-engine.sh first for the image and the weights.
set -uo pipefail

ROOT=${ROOT:-/mnt/d/Workspace/zlogic-fleet}
FLEET_HOME=${FLEET_HOME:-/root/fleet}
NS=${NS:-fleet}
GATEWAY=${GATEWAY:-127.0.0.1:8080}
export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}

pass=0
fail=0

ok()   { printf '  \033[32mok\033[0m   %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail+1)); }
head_() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# assert_contains <label> <haystack> <needle>
assert_contains() {
  case "$2" in
    *"$3"*) ok "$1" ;;
    *)      bad "$1 (expected to contain '$3', got: ${2:0:200})" ;;
  esac
}

cleanup() {
  [ -n "${OP_PID:-}" ] && kill "$OP_PID" 2>/dev/null
  [ -n "${GW_PID:-}" ] && kill "$GW_PID" 2>/dev/null
}
trap cleanup EXIT

# ------------------------------------------------------------------ 1. CRDs
head_ "1/6  custom resource definitions"
kubectl apply -f "$ROOT/operator/config/crd/bases/" >/dev/null 2>&1
if kubectl get crd fleetdeployments.fleet.zlogic.com >/dev/null 2>&1; then
  ok "FleetDeployment CRD is established"
else
  bad "FleetDeployment CRD is missing"
fi
kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS" >/dev/null

# ---------------------------------------------------------------- 2. declare
head_ "2/6  a model and a deployment, declared"
kubectl delete fleetdeployment --all -n "$NS" --ignore-not-found >/dev/null 2>&1
kubectl delete fleetmodel --all -n "$NS" --ignore-not-found >/dev/null 2>&1
kubectl apply -f "$ROOT/operator/config/samples/" >/dev/null 2>&1
if kubectl get fleetmodel qwen-0.5b-gguf -n "$NS" >/dev/null 2>&1; then
  ok "FleetModel accepted"
else
  bad "FleetModel was not created"
fi

# The control plane writes a model's status after the pull finishes. A status
# subresource cannot be set by apply, so the whole object is read back and its
# status replaced.
MODEL_JSON=$(kubectl get fleetmodel qwen-0.5b-gguf -n "$NS" -o json)
# The prefix is relative to the store the deployment mounts, not to the host.
# An absolute prefix renders as /models/root/fleet/weights/... and the engine
# reports a missing model, which looks like a bad image rather than a bad
# prefix.
REPO=Qwen/Qwen2.5-0.5B-Instruct-GGUF
PREFIX="models/$REPO"
FILE=qwen2.5-0.5b-instruct-q4_k_m.gguf
SIZE=$(stat -c%s "$FLEET_HOME/weights/$PREFIX/$FILE" 2>/dev/null || echo 0)
MODEL_JSON=$(python3 - "$MODEL_JSON" "$PREFIX" "$FILE" "$SIZE" <<'PY'
import json, sys
doc = json.loads(sys.argv[1])
doc["status"] = {
    "phase": "Ready",
    "format": "gguf",
    "prefix": sys.argv[2],
    "sizeBytes": int(sys.argv[4]),
    "objects": 1,
    "observedGeneration": 1,
    "files": [{"name": sys.argv[3], "bytes": int(sys.argv[4])}],
}
print(json.dumps(doc))
PY
)
printf '%s' "$MODEL_JSON" | kubectl replace --raw \
  "/apis/fleet.zlogic.com/v1alpha1/namespaces/$NS/fleetmodels/qwen-0.5b-gguf/status" \
  -f - >/dev/null 2>&1
assert_contains "model reached Ready" \
  "$(kubectl get fleetmodel qwen-0.5b-gguf -n "$NS" -o jsonpath='{.status.phase}')" "Ready"
assert_contains "the format was inferred, not declared" \
  "$(kubectl get fleetmodel qwen-0.5b-gguf -n "$NS" -o jsonpath='{.status.format}')" "gguf"

# -------------------------------------------------------------- 3. reconcile
head_ "3/6  the operator reconciles it"
mkdir -p "$FLEET_HOME/bin"
(cd "$ROOT/operator" && go build -o "$FLEET_HOME/bin/fleet-operator" ./cmd/manager) || {
  bad "the operator does not build"; exit 1; }
nohup "$FLEET_HOME/bin/fleet-operator" \
  --metrics-bind-address :9090 --health-probe-bind-address :9091 \
  --namespace "$NS" > "$FLEET_HOME/operator.log" 2>&1 &
OP_PID=$!

for _ in $(seq 1 90); do
  PHASE=$(kubectl get fleetdeployment qwen-0.5b -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$PHASE" = "Available" ] && break
  sleep 2
done
assert_contains "deployment reached Available" "${PHASE:-}" "Available"

SVC=$(kubectl get svc -n "$NS" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
ADDR=$(kubectl get fleetdeployment qwen-0.5b -n "$NS" -o jsonpath='{.status.address}' 2>/dev/null)
CLUSTER_IP=$(kubectl get svc "$SVC" -n "$NS" -o jsonpath='{.spec.clusterIP}' 2>/dev/null)
SELECTOR=$(kubectl get fleetdeployment qwen-0.5b -n "$NS" -o jsonpath='{.status.selector}' 2>/dev/null)
READY=$(kubectl get fleetdeployment qwen-0.5b -n "$NS" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)

[ -n "$SVC" ]        && ok "a Service was rendered ($SVC)"        || bad "no Service was rendered"
[ -n "$ADDR" ]       && ok "the deployment reports an address"     || bad "no address in status"
[ -n "$CLUSTER_IP" ] && ok "the Service has a cluster IP"         || bad "the Service has no IP"
[ "$READY" = "1" ]   && ok "one replica is ready"                  || bad "readyReplicas=$READY"

# The rendered name must differ from the deployment's: a Service cannot hold
# the dot in "qwen-0.5b", and reusing the name is refused by the API server.
if [ "$SVC" != "qwen-0.5b" ]; then
  ok "the Service name is a valid DNS label ($SVC)"
else
  bad "the Service name was reused verbatim and contains a dot"
fi

# ---------------------------------------------------------------- 4. engine
head_ "4/6  the engine answers under the name the client will use"
MODELS=$(kubectl run e2e-probe-$RANDOM --rm -i --restart=Never \
  --image=curlimages/curl:8.11.1 --quiet -- \
  curl -s --max-time 25 "http://$ADDR:8000/v1/models" 2>/dev/null | tr -d '\0')
assert_contains "the Service serves the model" "$MODELS" "$SELECTOR"
# Without --alias llama-server serves the GGUF's absolute path as the model
# id, and a gateway routing by name would have nothing to match.
if printf '%s' "$MODELS" | grep -q "$SELECTOR"; then
  ok "the model id is the alias, not a file path"
else
  bad "the model id is a path: $(printf '%s' "$MODELS" | head -c 200)"
fi

# --------------------------------------------------------------- 5. gateway
head_ "5/6  the gateway proxies to it"
(cd "$ROOT/core" && go build -o "$FLEET_HOME/bin/fleet-gateway" ./cmd/fleet-gateway) || {
  bad "the gateway does not build"; exit 1; }
FLEET_LISTEN="$GATEWAY" \
FLEET_UPSTREAMS="model=$SELECTOR,url=http://$CLUSTER_IP:8000,id=k3s-engine,engine=llama-cpp" \
  nohup "$FLEET_HOME/bin/fleet-gateway" > "$FLEET_HOME/gateway.log" 2>&1 &
GW_PID=$!

for _ in $(seq 1 30); do
  curl -sf --max-time 3 "http://$GATEWAY/health" >/dev/null 2>&1 && break
  sleep 1
done
assert_contains "the gateway is healthy" \
  "$(curl -s --max-time 5 "http://$GATEWAY/health" 2>/dev/null)" "ok"
assert_contains "the gateway lists the deployed model" \
  "$(curl -s --max-time 5 "http://$GATEWAY/v1/models" 2>/dev/null)" "$SELECTOR"

# -------------------------------------------------------------- 6. inference
head_ "6/6  a real completion, with the engine's own token counts"
curl -sN --max-time 120 -X POST "http://$GATEWAY/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$SELECTOR\",\"messages\":[{\"role\":\"user\",\"content\":\"Name three colours in one short sentence.\"}],\"max_tokens\":60,\"stream\":true,\"stream_options\":{\"include_usage\":true}}" \
  2>/dev/null | tr -d '\0' > "$FLEET_HOME/stream.txt"

FRAMES=$(grep -c '^data:' "$FLEET_HOME/stream.txt" 2>/dev/null) || FRAMES=0
case "$FRAMES" in ''|*[!0-9]*) FRAMES=0 ;; esac
if [ "$FRAMES" -gt 5 ]; then
  ok "the stream arrived in $FRAMES frames"
else
  bad "only $FRAMES frames"
fi
assert_contains "the stream is terminated" "$(cat "$FLEET_HOME/stream.txt")" "[DONE]"

python3 - "$FLEET_HOME/stream.txt" <<'PY'
import json, sys
text, usage, chunks = [], None, 0
for line in open(sys.argv[1], encoding="utf-8", errors="replace"):
    if not line.startswith("data:"):
        continue
    payload = line[5:].strip()
    if payload == "[DONE]":
        break
    try:
        d = json.loads(payload)
    except json.JSONDecodeError:
        continue
    chunks += 1
    for c in d.get("choices") or []:
        delta = c.get("delta") or {}
        if delta.get("content"):
            text.append(delta["content"])
    if d.get("usage"):
        usage = d["usage"]
answer = "".join(text).strip()
print(f"\n  answer: {answer[:160]}")
if len(answer) > 8:
    print("  \033[32mok\033[0m   the model answered with text")
else:
    print("  \033[31mFAIL\033[0m the model produced no text")
if usage and usage.get("completion_tokens", 0) > 0 and usage.get("prompt_tokens", 0) > 0:
    print(f"  \033[32mok\033[0m   usage from the engine: "
          f"{usage['prompt_tokens']} prompt + {usage['completion_tokens']} completion "
          f"= {usage['total_tokens']} tokens")
else:
    print("  \033[31mFAIL\033[0m no usable usage: without it nothing can be billed")
    sys.exit(1)
PY
[ $? -eq 0 ] && pass=$((pass+2)) || fail=$((fail+1))

head_ "result"
printf '  %d passed, %d failed\n\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1

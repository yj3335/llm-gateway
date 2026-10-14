#!/usr/bin/env bash
# End-to-end test of the Kubernetes deployment (deploy/k8s/overlays/local).
#   colima start --kubernetes           # or kind / Docker Desktop
#   docker compose build                # images are shared with the cluster runtime
#   kubectl apply -k deploy/k8s/overlays/local
#   scripts/k8s-smoke.sh
# Requires ingress-nginx and metrics-server in the cluster.
set -uo pipefail
cd "$(dirname "$0")/.."

NS=llm-gateway
HOST=gateway.example.com
ING_PORT=18081
WEB_PORT=13000
PASS=0
FAIL=0
TMP="$(mktemp -d)"
PIDS=()
cleanup() { for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null; wait "$p" 2>/dev/null; done; rm -rf "$TMP"; }
trap cleanup EXIT

ok()  { PASS=$((PASS + 1)); printf '  \033[32mPASS\033[0m %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf '  \033[31mFAIL\033[0m %s\n' "$1"; [ -n "${2:-}" ] && printf '       %s\n' "$2"; }
check() { if eval "$2"; then ok "$1"; else bad "$1" "${3:-}"; fi; }
k() { kubectl -n "$NS" "$@"; }
gw_pods() { k get pods -l app.kubernetes.io/name=llm-gateway --field-selector=status.phase=Running -o name; }
ready_gateways() { k get deploy llm-gateway -o jsonpath='{.status.readyReplicas}'; }
ICURL=(--connect-to "$HOST:80:127.0.0.1:$ING_PORT")

forward() { # forward <target> <local:remote> [namespace]
  kubectl -n "${3:-$NS}" port-forward "$1" "$2" >/dev/null 2>&1 &
  PIDS+=($!)
  disown $! 2>/dev/null || true
}

echo "== rollout"
for d in postgres redis mockllm llm-gateway llm-gateway-web; do
  k rollout status "deploy/$d" --timeout=240s >/dev/null 2>&1 || bad "deployment $d did not become available"
done
check "all deployments available; gateway has 3/3 ready replicas" '[ "$(ready_gateways)" = 3 ]' "ready=$(ready_gateways)"

echo "== pod security (production manifest)"
pod=$(gw_pods | head -1)
check "gateway runs as non-root UID 65532" '[ "$(k get $pod -o jsonpath="{.spec.securityContext.runAsUser}")" = 65532 ]'
check "read-only root filesystem, all capabilities dropped" '[ "$(k get $pod -o jsonpath="{.spec.containers[0].securityContext.readOnlyRootFilesystem}")" = true ] && [ "$(k get $pod -o jsonpath="{.spec.containers[0].securityContext.capabilities.drop[0]}")" = ALL ]'
check "GOMEMLIMIT/GOMAXPROCS derived from container limits" 'k get $pod -o yaml | grep "resource: limits.memory" >/dev/null'
degraded=0
# (grep without -q: with pipefail, an early grep exit SIGPIPEs kubectl and fails the pipeline)
for p in $(gw_pods); do k logs "$p" | grep '"cache_l2":"pgvector"' >/dev/null || degraded=$((degraded + 1)); done
check "every replica connected to Postgres (pgvector) and Redis" '[ "$degraded" = 0 ]'

echo "== port-forwards (ingress-nginx, web)"
forward svc/ingress-nginx-controller "$ING_PORT:80" ingress-nginx
forward svc/llm-gateway-web "$WEB_PORT:80"
# Wait for a real 200, not just any response: ingress-nginx answers 404/503
# for a few seconds until it has loaded a freshly applied Ingress.
code=000
for _ in $(seq 1 120); do
  code=$(curl -s "${ICURL[@]}" -o /dev/null -w "%{http_code}" "http://$HOST/readyz")
  [ "$code" = 200 ] && curl -sf -o /dev/null "http://localhost:$WEB_PORT/" && break
  sleep 1
done
check "Ingress routes $HOST to the gateway Service" '[ "$code" = 200 ]' "last status: $code"

echo "== functional smoke test through the Ingress (load-balanced across replicas)"
before=$(for p in $(gw_pods); do k logs "$p" | grep -c '"msg":"request"'; done | paste -sd' ' -)
GATEWAY_URL="http://$HOST" WEB_URL="http://localhost:$WEB_PORT" PROMETHEUS_URL=skip SMOKE_MULTI_REPLICA=1 \
  SMOKE_CURL_ARGS="--connect-to $HOST:80:127.0.0.1:$ING_PORT" \
  PSQL="kubectl -n $NS exec -i deploy/postgres -- psql -U llmgw -d llmgateway" \
  scripts/compose-smoke.sh > "$TMP/smoke.txt" 2>&1
grep -E "PASS|FAIL" "$TMP/smoke.txt" | sed 's/^/    /'
check "functional smoke test ($(grep -oE '[0-9]+ passed, [0-9]+ failed' "$TMP/smoke.txt"))" 'grep -q " 0 failed" $TMP/smoke.txt'
# Each replica exports its own counters; aggregate like Prometheus would
# (sum across pods), scraping through the API server's pod proxy.
for p in $(gw_pods); do kubectl get --raw "/api/v1/namespaces/$NS/pods/${p#pod/}:8080/proxy/metrics"; done > "$TMP/allmetrics.txt"
agg() { python3 - "$TMP/allmetrics.txt" "$1" <<'PY'
import sys
total = 0.0
for line in open(sys.argv[1]):
    if line.startswith(sys.argv[2]):
        total += float(line.rsplit(" ", 1)[1])
print(int(total))
PY
}
inj=$(agg 'llmgateway_guardrail_triggers_total{action="BLOCK",type="PROMPT_INJECTION"}')
sem=$(agg 'llmgateway_cache_lookups_total{result="HIT-SEMANTIC"}')
fo=$(agg 'llmgateway_provider_failovers_total')
check "metrics summed across pods: injection blocks=$inj, semantic hits=$sem, failovers=$fo" '[ "$inj" -ge 1 ] && [ "$sem" -ge 1 ] && [ "$fo" -ge 1 ]'
served=0
for p in $(gw_pods); do n=$(k logs "$p" | grep -c '"msg":"request"'); [ "$n" -gt 0 ] && served=$((served + 1)); done
check "Ingress load-balanced requests across $served/3 replicas (shared Redis/pgvector cache)" '[ "$served" = 3 ]' "before=$before"

echo "== autoscaling + disruption budget"
for _ in $(seq 1 60); do # metrics-server needs ~1-2 min after install to report
  cpu=$(k get hpa llm-gateway -o jsonpath='{.status.currentMetrics[0].resource.current.averageUtilization}' 2>/dev/null)
  [ -n "$cpu" ] && break; sleep 3
done
check "HPA reads live CPU metrics from metrics-server (current ${cpu:-?}% / target 65%)" '[ -n "$cpu" ]'
check "HPA bounds are 3..30 replicas" '[ "$(k get hpa llm-gateway -o jsonpath="{.spec.minReplicas}..{.spec.maxReplicas}")" = "3..30" ]'
check "PodDisruptionBudget allows exactly 1 voluntary disruption" '[ "$(k get pdb llm-gateway -o jsonpath="{.status.disruptionsAllowed}")" = 1 ]'

TOKEN=$(curl -s "${ICURL[@]}" -X POST "http://$HOST/v1/auth/token" -d '{"tenant_id":"k8s-load","rpm_limit":1000000,"tpm_limit":1000000000}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
# load <seconds> <outfile>: steady request loop through the Ingress, one status code per line
load() {
  local end=$((SECONDS + $1)) i=0
  while [ $SECONDS -lt $end ]; do
    i=$((i + 1))
    curl -s "${ICURL[@]}" -o /dev/null -w '%{http_code}\n' -X POST "http://$HOST/v1/chat/completions" \
      -H "Authorization: Bearer $TOKEN" -d "{\"model\":\"gpt-4o-mini\",\"messages\":[{\"role\":\"user\",\"content\":\"Explain topic $((i % 40)) in simple terms\"}]}" >> "$2"
    sleep 0.05
  done
}
summary() { local t; t=$(wc -l < "$1" | tr -d ' '); echo "$t requests, $(grep -cv '^200$' "$1") non-200"; }

echo "== self-healing: delete a gateway pod under load"
victim=$(gw_pods | head -1)
load 25 "$TMP/heal.txt" & LP=$!
sleep 3
k delete "$victim" --wait=false >/dev/null
for _ in $(seq 1 40); do [ "$(ready_gateways)" = 3 ] && ! k get "$victim" >/dev/null 2>&1 && break; sleep 1; done
wait $LP
check "deleted pod replaced; back to 3/3 ready" '[ "$(ready_gateways)" = 3 ]'
check "no failed requests while the pod was replaced ($(summary $TMP/heal.txt))" '! grep -qv "^200$" $TMP/heal.txt'

echo "== zero-downtime rolling restart"
load 45 "$TMP/roll.txt" & LP=$!
sleep 3
k rollout restart deploy/llm-gateway >/dev/null
k rollout status deploy/llm-gateway --timeout=180s >/dev/null 2>&1; rs=$?
wait $LP  # load keeps running through and after the rollout
check "rolling restart completed (all 3 replicas replaced)" '[ "$rs" = 0 ] && [ "$(ready_gateways)" = 3 ]'
check "zero failed requests during the rolling restart ($(summary $TMP/roll.txt))" '! grep -qv "^200$" $TMP/roll.txt'

echo "== graceful degradation: Redis outage"
k scale deploy/redis --replicas=0 >/dev/null
k wait --for=delete pod -l app=redis --timeout=60s >/dev/null 2>&1
sleep 2
hdrs=$(curl -s "${ICURL[@]}" -D - -o /dev/null -X POST "http://$HOST/v1/chat/completions" -H "Authorization: Bearer $TOKEN" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"redis outage probe '$RANDOM'"}]}')
check "requests still succeed with Redis down (HTTP 200)" 'echo "$hdrs" | head -1 | grep -q " 200"'
check "rate limiter falls back to local buckets (X-RateLimit-Mode: local-fallback)" 'echo "$hdrs" | grep -qi "X-RateLimit-Mode: local-fallback"'
k scale deploy/redis --replicas=1 >/dev/null
k rollout status deploy/redis --timeout=120s >/dev/null 2>&1
recovered=no
for _ in $(seq 1 20); do
  h=$(curl -s "${ICURL[@]}" -D - -o /dev/null -X POST "http://$HOST/v1/chat/completions" -H "Authorization: Bearer $TOKEN" \
    -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"redis recovery probe '$RANDOM'"}]}')
  echo "$h" | head -1 | grep -q " 200" && ! echo "$h" | grep -qi "local-fallback" && { recovered=yes; break; }
  sleep 2
done
check "gateway returns to Redis-backed rate limiting after recovery" '[ "$recovered" = yes ]'

echo
echo "== $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]

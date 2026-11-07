#!/usr/bin/env bash
# Smoke test of agentops-operator, ai-agent-edge-tunnel and this client together on a kind cluster.
#
# It needs no Cloudflare account: the edge operator gets a fake token, so everything that talks to
# Cloudflare (tunnel, DNS, Access application, connector, public URL) is NOT covered. It covers the
# cluster half: installs, CRDs, binding, sessionRef waiting and pinning, termination, RBAC, access-policy refusal
# and the client's Expose/Complete.
#
# Environment:
#   OPERATOR_DIR  checkout of agentops-operator   (default ../agentops-operator)
#   EDGE_DIR      checkout of ai-agent-edge-tunnel (default ../ai-agent-edge-tunnel)
#   CLUSTER       kind cluster name                (default agentops-smoke)
# Needs docker, kind, kubectl, kustomize and go on PATH.
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
OPERATOR_DIR="$(cd "${OPERATOR_DIR:-$HERE/../agentops-operator}" && pwd)"
EDGE_DIR="$(cd "${EDGE_DIR:-$HERE/../ai-agent-edge-tunnel}" && pwd)"
CLUSTER="${CLUSTER:-agentops-smoke}"
NS=smoke
OP_IMG=agentops-operator:ci
EDGE_IMG=ai-agent-edge-tunnel:ci
AGENT_IMG=echo-agent:ci

pass=0
step() { printf '\n== %s\n' "$*"; }
ok() { printf 'PASS: %s\n' "$*"; pass=$((pass + 1)); }
fail() { printf 'FAIL: %s\n' "$*" >&2; diagnose; exit 1; }

diagnose() {
  echo "---- diagnostics ----" >&2
  kubectl get agentsessions,agentedgetunnels,agentservices,agentpools -A -o wide >&2 || true
  kubectl -n "$NS" get agentedgetunnels -o yaml >&2 || true
  kubectl -n agentops-operator-system logs deploy/agentops-operator-controller-manager --tail=80 >&2 || true
  kubectl -n edge-tunnel-system logs deploy/edge-tunnel-controller-manager --tail=80 >&2 || true
  kubectl get pods -A >&2 || true
}

# field NAME KIND JSONPATH prints a field of an object in $NS, or nothing.
field() { kubectl -n "$NS" get "$2" "$1" -o "jsonpath=$3" 2>/dev/null || true; }

# wait_field NAME KIND JSONPATH WANT TIMEOUT_SECONDS
wait_field() {
  local end=$((SECONDS + $5)) got=""
  while [ "$SECONDS" -lt "$end" ]; do
    got="$(field "$1" "$2" "$3")"
    [ "$got" = "$4" ] && return 0
    sleep 2
  done
  echo "timed out: $2/$1 $3 is '$got', wanted '$4'" >&2
  return 1
}

step "cluster"
if ! kind get clusters | grep -qx "$CLUSTER"; then kind create cluster --name "$CLUSTER" --wait 120s; fi
kubectl cluster-info >/dev/null

step "build and load images"
docker build -t "$OP_IMG" "$OPERATOR_DIR"
docker build -f "$OPERATOR_DIR/examples/echo-agent/Dockerfile" -t "$AGENT_IMG" "$OPERATOR_DIR"
docker build -t "$EDGE_IMG" "$EDGE_DIR"
for img in "$OP_IMG" "$AGENT_IMG" "$EDGE_IMG"; do kind load docker-image "$img" --name "$CLUSTER"; done

step "install agentops-operator"
(cd "$OPERATOR_DIR" && make install deploy IMG="$OP_IMG")
kubectl -n agentops-operator-system patch deploy agentops-operator-controller-manager --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]'
kubectl -n agentops-operator-system rollout status deploy/agentops-operator-controller-manager --timeout=180s
ok "agentops-operator is running"

step "install ai-agent-edge-tunnel (fake Cloudflare credentials)"
kubectl create namespace edge-tunnel-system --dry-run=client -o yaml | kubectl apply -f -
kubectl -n edge-tunnel-system create secret generic edge-tunnel-cloudflare \
  --from-literal=account-id=smoke --from-literal=api-token=smoke --dry-run=client -o yaml | kubectl apply -f -
kubectl -n edge-tunnel-system create configmap edge-tunnel-config \
  --from-literal=EDGE_BASE_DOMAIN=agents.example.test \
  --from-literal=CONNECTOR_IMAGE=cloudflare/cloudflared:2024.1.0 --dry-run=client -o yaml | kubectl apply -f -
(cd "$EDGE_DIR" && make deploy IMG="$EDGE_IMG")
kubectl -n edge-tunnel-system patch deploy edge-tunnel-controller-manager --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]'
kubectl -n edge-tunnel-system rollout status deploy/edge-tunnel-controller-manager --timeout=180s
ok "ai-agent-edge-tunnel is running"

step "CRDs from both groups are served"
kubectl get crd agentsessions.agents.steadyinfra.com agentedgetunnels.edge.steadyinfra.com >/dev/null
ok "CRDs installed"

step "agent pool"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NS" apply -f "$HERE/hack/kind-smoke/agents.yaml"
# The smoke namespace holds only the agent's Deployment, so wait on that rather than on pods.
end=$((SECONDS + 60))
until kubectl -n "$NS" get deploy -o name 2>/dev/null | grep -q .; do
  [ "$SECONDS" -lt "$end" ] || fail "the agent Deployment was never created"
  sleep 2
done
kubectl -n "$NS" wait --for=condition=Available deployment --all --timeout=180s || fail "the agent Deployment never became Available"
ok "agent pod is ready"

step "RBAC: the edge operator may read sessions, not change them"
SA=system:serviceaccount:edge-tunnel-system:edge-tunnel-controller-manager
[ "$(kubectl auth can-i get agentsessions.agents.steadyinfra.com -n "$NS" --as "$SA")" = yes ] || fail "edge operator cannot get agentsessions"
for verb in create update patch delete; do
  [ "$(kubectl auth can-i "$verb" agentsessions.agents.steadyinfra.com -n "$NS" --as "$SA")" = no ] || fail "edge operator can $verb agentsessions"
done
ok "edge operator RBAC on agentsessions is get-only"

step "validation: a tunnel is never public by omission"
# The CRD only rejects allowedEmails together with allowPublic; the controller refuses a tunnel with
# neither, so this shows up as Failed in status, before anything is created in Cloudflare.
kubectl -n "$NS" apply -f - <<'E2'
apiVersion: edge.steadyinfra.com/v1alpha1
kind: AgentEdgeTunnel
metadata: {name: nosecurity}
spec:
  service: {name: echo, port: 8080}
  cloudflare: {zoneID: zone, subdomain: nosecurity}
E2
wait_field nosecurity agentedgetunnel '{.status.phase}' Failed 60 || fail "a tunnel with no access rule was not marked Failed"
case "$(field nosecurity agentedgetunnel '{.status.message}')" in
  *"no access policy"*) ;;
  *) fail "Failed for the wrong reason: $(field nosecurity agentedgetunnel '{.status.message}')" ;;
esac
[ -z "$(field nosecurity agentedgetunnel '{.status.tunnelID}')" ] || fail "a Cloudflare tunnel was created for an unprotected spec"
kubectl -n "$NS" delete agentedgetunnel nosecurity --wait=true
ok "tunnel without allowedEmails or allowPublic is refused before anything is created"

step "sessionRef waits for a session that does not exist yet"
kubectl -n "$NS" apply -f - <<'E2'
apiVersion: edge.steadyinfra.com/v1alpha1
kind: AgentEdgeTunnel
metadata: {name: early}
spec:
  sessionRef: {name: early}
  cloudflare: {zoneID: zone, subdomain: early}
  security: {allowedEmails: [smoke@example.com]}
E2
wait_field early agentedgetunnel '{.status.message}' 'waiting for AgentSession "early" to exist' 120 || fail "tunnel did not report waiting for the session"
sleep 10
[ -z "$(field early agentedgetunnel '{.status.sessionUID}')" ] || fail "tunnel pinned a session that does not exist"
ok "tunnel waits, nothing pinned"

step "session binds and the waiting tunnel pins it"
kubectl -n "$NS" apply -f - <<'E2'
apiVersion: agents.steadyinfra.com/v1alpha1
kind: AgentSession
metadata: {name: early}
spec:
  poolRef: {name: smoke}
  userID: smoke
E2
wait_field early agentsession '{.status.phase}' Bound 120 || fail "session never became Bound"
uid="$(field early agentsession '{.metadata.uid}')"
endpoint="$(field early agentsession '{.status.endpoint}')"
wait_field early agentedgetunnel '{.status.sessionUID}' "$uid" 120 || fail "tunnel did not pin the session UID"
[ "$(field early agentedgetunnel '{.status.sessionEndpoint}')" = "$endpoint" ] || fail "tunnel pinned a different endpoint than the session reports"
ok "session Bound ($endpoint) and pinned by the tunnel"

step "completing the session ends the tunnel"
kubectl -n "$NS" patch agentsession early --type merge -p '{"spec":{"completed":true}}'
wait_field early agentedgetunnel '{.status.phase}' Terminated 120 || fail "tunnel did not terminate after the session completed"
ok "tunnel Terminated after session completion"

step "client: Expose creates both objects, then Complete ends the session"
# Cloudflare is fake, so Wait cannot succeed: the example must time out, clean up and exit non-zero.
if (cd "$HERE" && go run ./examples/smoke -namespace "$NS" -name viaclient -pool smoke \
    -zone-id zone -email smoke@example.com -timeout 45s); then
  fail "the smoke example succeeded although Cloudflare is fake"
fi
owner="$(kubectl -n "$NS" get agentedgetunnel viaclient -o jsonpath='{.metadata.ownerReferences[0].kind}')"
[ "$owner" = AgentSession ] || fail "tunnel created by the client has no AgentSession ownerReference (got '$owner')"
[ "$(field viaclient agentsession '{.spec.completed}')" = true ] || fail "the client did not complete the session after Wait failed"
suid="$(field viaclient agentsession '{.metadata.uid}')"
wait_field viaclient agentedgetunnel '{.status.sessionUID}' "$suid" 60 || fail "client-created tunnel did not pin its session"
ok "client created session and owned tunnel, and completed the session after the failed wait"

step "deleting the session deletes its tunnel"
kubectl -n "$NS" delete agentsession viaclient --wait=true
end=$((SECONDS + 60))
while kubectl -n "$NS" get agentedgetunnel viaclient >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$end" ] || fail "tunnel outlived its deleted session"
  sleep 2
done
ok "tunnel garbage-collected with its session"

printf '\nAll %d checks passed. Not covered: anything that needs real Cloudflare.\n' "$pass"

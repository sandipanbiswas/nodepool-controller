#!/usr/bin/env bash
# Demo: create pool → simulate join → labels/taints applied → spoof NotReady → node removed → delete pool.
# Uses a *synthetic* Node object so we never drain a real kind worker.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

POOL="${POOL:-gpu-pool}"
NODE="${NODE:-demo-gpu-node}"
NS="${NS:-nodepool-controller-system}"

need() { command -v "$1" >/dev/null || { echo "missing $1"; exit 1; }; }
need kubectl
need go

echo "==> Install CRDs"
make install

echo "==> Start controller in-process (make run) if one is not already deployed"
if ! kubectl get deploy -n "$NS" controller-manager >/dev/null 2>&1; then
  echo "No in-cluster controller found; starting 'make run' in the background."
  echo "Set SKIP_RUN=1 if you already have 'make run' in another terminal."
  if [[ "${SKIP_RUN:-}" != "1" ]]; then
    make run >/tmp/nodepool-controller-demo.log 2>&1 &
    RUN_PID=$!
    trap 'kill "$RUN_PID" 2>/dev/null || true' EXIT
    sleep 5
  fi
fi

echo "==> Create NodePool"
kubectl apply -f config/samples/nodes_v1alpha1_nodepool.yaml

echo "==> Simulate infra join: create a synthetic Node and stamp the bootstrap label"
kubectl apply -f - <<EOF
apiVersion: v1
kind: Node
metadata:
  name: ${NODE}
  labels:
    nodes.example.com/nodepool: ${POOL}
    kubernetes.io/hostname: ${NODE}
spec: {}
status:
  conditions:
    - type: Ready
      status: "True"
      reason: KubeletReady
      message: synthetic
      lastHeartbeatTime: "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
      lastTransitionTime: "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
EOF

echo "==> Wait for labels/taints"
for i in $(seq 1 30); do
  if kubectl get node "$NODE" -o jsonpath='{.metadata.labels.workload}' | grep -q gpu; then
    break
  fi
  sleep 1
done
kubectl get node "$NODE" --show-labels
kubectl get node "$NODE" -o jsonpath='{.spec.taints}' ; echo
kubectl get nodepool "$POOL" -o yaml

echo "==> Simulate infra reclaim: Ready=Unknown (kubelet gone)"
kubectl patch node "$NODE" --subresource=status --type=json -p='[
  {"op":"replace","path":"/status/conditions","value":[
    {"type":"Ready","status":"Unknown","reason":"NodeStatusUnknown","message":"kubelet stopped posting",
     "lastHeartbeatTime":"'"$(date -u +%Y-%m-%dT%H:%M:%SZ)"'",
     "lastTransitionTime":"'"$(date -u +%Y-%m-%dT%H:%M:%SZ)"'"}
  ]}
]'

echo "==> Temporarily shorten grace? Sample uses 5m. For demo, patch gracePeriod to 5s."
kubectl patch nodepool "$POOL" --type=merge -p '{"spec":{"unreadyPolicy":{"gracePeriod":"5s","action":"Remove","maxConcurrentRemovals":1}}}'

echo "==> Wait for controller to delete the Node"
for i in $(seq 1 40); do
  if ! kubectl get node "$NODE" >/dev/null 2>&1; then
    echo "Node ${NODE} removed"
    break
  fi
  kubectl get nodepool "$POOL" -o jsonpath='{.status.nodes}' ; echo
  sleep 1
done

echo "==> Recreate a Ready member then delete the pool (label cleanup)"
kubectl apply -f - <<EOF
apiVersion: v1
kind: Node
metadata:
  name: ${NODE}
  labels:
    nodes.example.com/nodepool: ${POOL}
    human: keep-me
spec: {}
status:
  conditions:
    - type: Ready
      status: "True"
      reason: KubeletReady
      message: synthetic
      lastHeartbeatTime: "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
      lastTransitionTime: "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
EOF
sleep 3
kubectl delete nodepool "$POOL" --wait=true
echo "Remaining labels on ${NODE}:"
kubectl get node "$NODE" --show-labels || true
kubectl delete node "$NODE" --ignore-not-found
echo "==> Done. Events:"
kubectl get events --field-selector involvedObject.kind=NodePool --sort-by=.lastTimestamp | tail -n 20

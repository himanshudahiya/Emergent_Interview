# Pod Config Operator — Deployment & Testing Guide

## Prerequisites

- Go 1.22+
- kubectl configured against a cluster (minikube / kind / real cluster)
- Docker (or podman) for building the image
- kubebuilder v3+ (optional, for codegen)

---

## Step 1 — Bootstrap the Go module

```bash
# Create the project root
mkdir -p pod-config-operator && cd pod-config-operator

# Copy all generated files into place (api/, internal/, cmd/, config/)
# Then tidy dependencies
go mod tidy
```

---

## Step 2 — Generate DeepCopy methods

controller-runtime requires DeepCopy methods on all API types.
Run the generator once after any change to types.go:

```bash
# Install the generator if not present
go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest

# Generate DeepCopyObject implementations
controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

# (Optional) Also regenerate the CRD YAML from Go struct markers
controller-gen crd paths="./api/..." output:crd:artifacts:config=config/crd
```

---

## Step 3 — Install the CRD

```bash
kubectl apply -f config/crd/globalpodconfig_crd.yaml

# Verify
kubectl get crd globalpodconfigs.config.example.com
```

---

## Step 4 — Create the operator namespace and RBAC

```bash
kubectl create namespace pod-config-operator-system

kubectl apply -f config/samples/globalpodconfig_sample.yaml  # contains SA + ClusterRole + CRB
```

---

## Step 5 — Run the operator

### Option A — Run locally (outside cluster, fastest for dev)

```bash
go run ./cmd/main.go
# The operator uses your local kubeconfig (~/.kube/config)
```

### Option B — Build and deploy inside the cluster

```bash
# Build image
docker build -t pod-config-operator:latest .

# If using kind, load image directly (no registry needed)
kind load docker-image pod-config-operator:latest

# Apply the operator Deployment
kubectl apply -f config/deploy/operator_deployment.yaml
```

**Dockerfile** (`Dockerfile` at project root):

```dockerfile
FROM golang:1.22 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o manager ./cmd/main.go

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/manager /manager
ENTRYPOINT ["/manager"]
```

**Operator Deployment** (`config/deploy/operator_deployment.yaml`):

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: pod-config-operator
  namespace: pod-config-operator-system
spec:
  replicas: 1
  selector:
    matchLabels:
      app: pod-config-operator
  template:
    metadata:
      labels:
        app: pod-config-operator
    spec:
      serviceAccountName: pod-config-operator
      containers:
        - name: manager
          image: pod-config-operator:latest
          imagePullPolicy: IfNotPresent
          args:
            - --leader-elect=false
          ports:
            - containerPort: 8080 # metrics
            - containerPort: 8081 # health probe
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8081
          readinessProbe:
            httpGet:
              path: /readyz
              port: 8081
```

---

## Step 6 — Watch operator logs

```bash
# Local run: logs go to stdout
# In-cluster:
kubectl logs -f -n pod-config-operator-system deploy/pod-config-operator
```

---

# Test Cases

Each test case below follows the same pattern:

1. Create the test namespace + nginx pod(s)
2. Create or update the GlobalPodConfig CR
3. Observe what the operator does
4. Assert the expected state

---

## Test 1 — Baseline: env var injection (requires eviction + recreate)

```bash
# 1. Create namespace and a plain nginx pod (no env vars)
kubectl create namespace test-nginx

kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: nginx-baseline
  namespace: test-nginx
  labels:
    app: nginx
spec:
  containers:
    - name: nginx
      image: nginx:1.25
      resources:
        requests:
          cpu: "50m"
          memory: "64Mi"
EOF

# 2. Apply a GlobalPodConfig that injects LOG_LEVEL
kubectl apply -f - <<EOF
apiVersion: config.example.com/v1alpha1
kind: GlobalPodConfig
metadata:
  name: nginx-global-config
spec:
  labelSelector:
    matchLabels:
      app: nginx
  podTemplate:
    env:
      - name: LOG_LEVEL
        value: "info"
  evictionPolicy:
    strategy: Evict
    gracePeriodSeconds: 5
  syncInterval: "15s"
EOF

# 3. Watch the pod — it should be evicted and recreated
kubectl get pods -n test-nginx -w

# 4. Assert env var is present on the recreated pod
kubectl exec -n test-nginx nginx-baseline -- env | grep LOG_LEVEL
# Expected: LOG_LEVEL=info

# 5. Check operator status
kubectl get globalpodconfig nginx-global-config
# Expected: UPDATED=1
```

---

## Test 2 — Mutable fields only: label and annotation patch (no eviction)

```bash
# Reuse namespace from Test 1; pod is already running

# Update the CR to add a label and annotation
kubectl patch globalpodconfig nginx-global-config --type=merge -p '
{
  "spec": {
    "podTemplate": {
      "labels": {
        "config.example.com/managed": "true"
      },
      "annotations": {
        "config.example.com/managed-by": "pod-config-operator"
      }
    }
  }
}'

# Wait one sync cycle (15s) or trigger immediately by touching the CR
kubectl annotate globalpodconfig nginx-global-config force-sync="$(date +%s)" --overwrite

# Assert — pod should NOT be evicted, just patched in-place
kubectl get pod nginx-baseline -n test-nginx --show-labels
# Expected: label config.example.com/managed=true is present

kubectl get pod nginx-baseline -n test-nginx -o jsonpath='{.metadata.annotations}'
# Expected: annotation config.example.com/managed-by=pod-config-operator present
```

---

## Test 3 — Multi-namespace scan

```bash
# Create a second namespace with another nginx pod
kubectl create namespace test-nginx-2

kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: nginx-ns2
  namespace: test-nginx-2
  labels:
    app: nginx
spec:
  containers:
    - name: nginx
      image: nginx:1.25
EOF

# Wait for the next sync (15s). The operator scans ALL namespaces.
sleep 20

# Assert both pods have LOG_LEVEL
kubectl exec -n test-nginx  nginx-baseline -- env | grep LOG_LEVEL
kubectl exec -n test-nginx-2 nginx-ns2     -- env | grep LOG_LEVEL
# Both should return: LOG_LEVEL=info

# Check matched count on the CR
kubectl get globalpodconfig nginx-global-config -o jsonpath='{.status.matchedPods}'
# Expected: 2
```

---

## Test 4 — Label selector specificity (non-matching pods are untouched)

```bash
# Create a pod with a DIFFERENT label — should be ignored
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: not-nginx
  namespace: test-nginx
  labels:
    app: apache        # does NOT match selector app=nginx
spec:
  containers:
    - name: httpd
      image: httpd:2.4
EOF

sleep 20  # wait for sync

# Assert: pod has no LOG_LEVEL (operator left it alone)
kubectl exec -n test-nginx not-nginx -- env | grep LOG_LEVEL || echo "PASS: env not injected"

# Assert: matched count is still 2 (not 3)
kubectl get globalpodconfig nginx-global-config -o jsonpath='{.status.matchedPods}'
# Expected: 2
```

---

## Test 5 — Resource limits enforcement (immutable → eviction)

```bash
# Apply resource limits via the CR
kubectl patch globalpodconfig nginx-global-config --type=merge -p '
{
  "spec": {
    "podTemplate": {
      "resources": {
        "limits":   { "cpu": "200m", "memory": "128Mi" },
        "requests": { "cpu": "100m", "memory": "64Mi"  }
      }
    }
  }
}'

# Watch: pods should be evicted and recreated with new limits
kubectl get pods -n test-nginx -w
kubectl get pods -n test-nginx-2 -w

# Assert resource limits on recreated pod
kubectl get pod nginx-baseline -n test-nginx \
  -o jsonpath='{.spec.containers[0].resources}' | jq .
# Expected: limits.cpu=200m, limits.memory=128Mi
```

---

## Test 6 — Drift detection (manual edit undone by operator)

```bash
# Manually add a rogue env var by evicting and recreating with a wrong value
# Simulate this by patching the pod directly to change LOG_LEVEL
# (In practice this could be a human mistake or another controller.)

# First, get the current pod and delete/recreate it with wrong env
kubectl delete pod nginx-baseline -n test-nginx
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: nginx-baseline
  namespace: test-nginx
  labels:
    app: nginx
spec:
  containers:
    - name: nginx
      image: nginx:1.25
      env:
        - name: LOG_LEVEL
          value: "debug"    # WRONG — should be "info"
EOF

# Wait for operator sync (up to 15s)
sleep 20

# Assert: operator detected drift and corrected it
kubectl exec -n test-nginx nginx-baseline -- env | grep LOG_LEVEL
# Expected: LOG_LEVEL=info  (operator wins)
```

---

## Test 7 — evictionPolicy=Skip (operator logs but does not evict)

```bash
# Switch strategy to Skip
kubectl patch globalpodconfig nginx-global-config --type=merge -p '
{
  "spec": {
    "evictionPolicy": { "strategy": "Skip" }
  }
}'

# Now introduce drift (delete and recreate with wrong env)
kubectl delete pod nginx-baseline -n test-nginx
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: nginx-baseline
  namespace: test-nginx
  labels:
    app: nginx
spec:
  containers:
    - name: nginx
      image: nginx:1.25
      env:
        - name: LOG_LEVEL
          value: "debug"
EOF

sleep 20

# Assert: pod is NOT evicted, LOG_LEVEL stays wrong
kubectl exec -n test-nginx nginx-baseline -- env | grep LOG_LEVEL
# Expected: LOG_LEVEL=debug  (operator skipped it)

# Assert: skippedPods count incremented
kubectl get globalpodconfig nginx-global-config -o jsonpath='{.status.skippedPods}'
# Expected: 1

# Reset strategy back
kubectl patch globalpodconfig nginx-global-config --type=merge -p '
{"spec":{"evictionPolicy":{"strategy":"Evict"}}}'
```

---

## Test 8 — Periodic resync without CR changes

```bash
# Delete the pod manually (simulates a crash/node failure scenario)
kubectl delete pod nginx-baseline -n test-nginx

# Recreate it WITHOUT the desired env (simulates controller recreating it bare)
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: nginx-baseline
  namespace: test-nginx
  labels:
    app: nginx
spec:
  containers:
    - name: nginx
      image: nginx:1.25
EOF

# Do NOT touch the GlobalPodConfig CR — just wait for periodic sync
sleep 20

# Assert: env was enforced on the next tick even without a CR event
kubectl exec -n test-nginx nginx-baseline -- env | grep LOG_LEVEL
# Expected: LOG_LEVEL=info
```

---

## Test 9 — GlobalPodConfig deletion (operator stops managing pods)

```bash
kubectl delete globalpodconfig nginx-global-config

# Recreate a pod without desired env — operator should do nothing
kubectl delete pod nginx-baseline -n test-nginx
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: nginx-baseline
  namespace: test-nginx
  labels:
    app: nginx
spec:
  containers:
    - name: nginx
      image: nginx:1.25
EOF

sleep 20

kubectl exec -n test-nginx nginx-baseline -- env | grep LOG_LEVEL || echo "PASS: not managed"
# Expected: PASS: not managed
```

---

## Test 10 — Status subresource verification

```bash
# Reapply the CR from Test 1 and check all status fields
kubectl apply -f config/samples/globalpodconfig_sample.yaml

sleep 20

kubectl get globalpodconfig nginx-global-config -o yaml | grep -A 20 'status:'
# Expected fields:
#   matchedPods: <N>
#   updatedPods: <N>
#   skippedPods: 0
#   lastSyncTime: <recent timestamp>
#   conditions:
#     - type: Ready
#       status: "True"
#       reason: ReconcileComplete
```

---

## Cleanup

```bash
kubectl delete globalpodconfig nginx-global-config
kubectl delete namespace test-nginx test-nginx-2
kubectl delete clusterrole pod-config-operator
kubectl delete clusterrolebinding pod-config-operator
kubectl delete namespace pod-config-operator-system
kubectl delete crd globalpodconfigs.config.example.com
```

# Pod Sequence Controller - Lite Mode

This is a simplified deployment that **only supports cluster-scoped sequencing**. No webhook, no certificates, no node taints - just simple gate-based sequencing.

## What's Supported

✅ **Cluster-scoped sequencing**: Wait for all pods in Group N to be ready before starting Group N+1  
❌ **Node-scoped sequencing**: Not supported (requires full deployment with webhook)

## Deployment

### 1. Install CRD
```bash
kubectl apply -f config/crd/podsequence-crd.yaml
```

### 2. Deploy Controller (Lite Mode)
```bash
kubectl apply -f config/rbac/rbac-lite.yaml
```

That's it! No certificates, no webhook configuration needed.

## Usage Example

```yaml
apiVersion: scheduling.example.com/v1alpha1
kind: PodSequence
metadata:
  name: my-sequence
spec:
  scope: Cluster  # ← Required (or omit, defaults to Cluster)
  podGroups:
  - name: "Database"
    pods: ["postgres-0", "postgres-1"]
  - name: "App Servers"
    podSelector:
      matchLabels:
        app: myapp
---
apiVersion: v1
kind: Pod
metadata:
  name: postgres-0
spec:
  schedulingGates:
  - name: podsequence.example.com/sequence-gate
  containers:
  - name: postgres
    image: postgres:14
```

## What Happens if You Try Node-Scoped?

The controller will immediately fail the PodSequence with this message:
```
Phase: Failed
Message: Node-scoped sequencing requires webhook to be enabled. Use scope: Cluster or redeploy with --enable-webhook flag.
```

## When to Use Full Deployment

Use the full deployment (with webhook) if you need:
- **Per-node sequencing**: Different sequencing on each node
- **DaemonSet-like workloads**: Node-level dependencies

## Comparison

| Feature | Lite Mode | Full Mode |
|---------|-----------|-----------|
| Cluster-scoped sequencing | ✅ | ✅ |
| Node-scoped sequencing | ❌ | ✅ |
| Webhook required | ❌ | ✅ |
| Certificates required | ❌ | ✅ |
| Node permissions required | ❌ | ✅ |
| Deployment complexity | Simple | Complex |

## Upgrading to Full Mode

To enable node-scoped sequencing later:

1. Generate certificates:
```bash
./config/webhook/generate-certs.sh
```

2. Deploy webhook configuration:
```bash
kubectl apply -f config/webhook/webhook.yaml
```

3. Update deployment to enable webhook:
```bash
kubectl apply -f config/rbac/rbac.yaml  # Full version with --enable-webhook
```

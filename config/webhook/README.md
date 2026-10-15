# Webhook Deployment Guide

The webhook is required for **node-scoped** sequencing ("full mode"). It has two handlers:

| Path | Objects | What it does |
|---|---|---|
| `/mutate-v1-pod` | Pods (CREATE) | Adds the PodSequence scheduling gate to pods labeled `podsequence.example.com/sequence: <podsequence-name>` that match one of the sequence's groups. |
| `/mutate-apps-v1-daemonset` | DaemonSets (CREATE/UPDATE) | Copies the `podsequence.example.com/sequence` label into the pod template, and in `Taint` mode adds a toleration for the sequence's node taint **to the pod template**. The DaemonSet controller checks template tolerations before it creates a pod, so pod-level tolerations would be too late. |

Both webhooks use an `objectSelector` on the `podsequence.example.com/sequence` label, so only opted-in objects are ever sent to the webhook. No namespace label is required.

## Prerequisites
1. Ensure the controller Docker image is built and pushed
2. OpenSSL installed (for certificate generation)

## Step 1: Generate Certificates
Run the certificate generation script:
```bash
chmod +x config/webhook/generate-certs.sh
./config/webhook/generate-certs.sh
```

This creates a secret `webhook-server-cert` and outputs a command to patch the webhook configuration.

## Step 2: Deploy the Webhook Configuration and Service
```bash
kubectl apply -f config/webhook/webhook.yaml
```

## Step 3: Patch the Webhook with CA Bundle
Use the command output from Step 1 (it patches both webhooks), or manually:
```bash
kubectl patch mutatingwebhookconfiguration podsequence-mutating-webhook --type='json' -p='[
  {"op": "replace", "path": "/webhooks/0/clientConfig/caBundle", "value":"<CA_BUNDLE>"},
  {"op": "replace", "path": "/webhooks/1/clientConfig/caBundle", "value":"<CA_BUNDLE>"}]'
```

## Step 4: Deploy/Restart the Controller
```bash
kubectl apply -f config/rbac/rbac.yaml
kubectl rollout restart deployment pod-sequence-controller -n pod-sequence-system
```

## Step 5: Try a Sample
```bash
kubectl apply -f config/samples/node-scoped-csi-example.yaml
```

## Opting In

Add the label to the DaemonSet/Deployment **pod template** (and to the DaemonSet metadata so the DaemonSet webhook sees it):

```yaml
metadata:
  labels:
    podsequence.example.com/sequence: csi-driver-sequence   # DaemonSet webhook (Taint mode)
spec:
  template:
    metadata:
      labels:
        podsequence.example.com/sequence: csi-driver-sequence # pod webhook (gate injection)
```

- Create the `PodSequence` **before** the workloads. Pods created while the PodSequence does not exist are not gated (the webhook returns a warning).
- Adding the label to an existing DaemonSet changes its pod template and triggers a rolling update.
- Pods with the annotation `podsequence.example.com/bypass: "true"` are never gated (and are released by the controller if already gated).

## Failure Policy

`failurePolicy: Ignore` (default) means that if the webhook is unavailable, member pods are created **without** a gate and start unordered. Use `Fail` to block member pod creation instead; because of the `objectSelector`, only opted-in pods are affected either way.

## AKS Note

AKS's admissions enforcer skips webhooks for `kube-system` unless the webhook configuration has the label `admissions.enforcer/disabled: "true"`. Run sequenced workloads in another namespace, or add that label if you need `kube-system`.

## Troubleshooting

### Check webhook logs:
```bash
kubectl logs -n pod-sequence-system -l app=pod-sequence-controller | grep -i webhook
```

### Verify webhook configuration:
```bash
kubectl get mutatingwebhookconfiguration podsequence-mutating-webhook -o yaml
```

### Check certificate secret:
```bash
kubectl get secret webhook-server-cert -n pod-sequence-system
```

### Pod was not gated
- Is the `podsequence.example.com/sequence` label on the **pod** (template) and does its value match the PodSequence name?
- Does the pod match one of the sequence's `podSelector`s?
- `kubectl get events` for webhook warnings.
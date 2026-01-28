# Webhook Deployment Guide

## Prerequisites
1. Ensure the controller Docker image is built and pushed
2. OpenSSL installed (for certificate generation)

## Step 1: Label the Namespace
The webhook only affects pods in labeled namespaces:
```bash
kubectl label namespace default podsequence-enabled=true
```

## Step 2: Generate Certificates
Run the certificate generation script:
```bash
chmod +x config/webhook/generate-certs.sh
./config/webhook/generate-certs.sh
```

This creates a secret `webhook-server-cert` and outputs a command to patch the webhook configuration.

## Step 3: Deploy the Webhook Configuration and Service
```bash
kubectl apply -f config/webhook/webhook.yaml
```

## Step 4: Patch the Webhook with CA Bundle
Use the command output from Step 2, or manually:
```bash
kubectl patch mutatingwebhookconfiguration podsequence-mutating-webhook \
  --type='json' \
  -p='[{"op": "replace", "path": "/webhooks/0/clientConfig/caBundle", "value":"<CA_BUNDLE>"}]'
```

## Step 5: Deploy/Restart the Controller
```bash
kubectl apply -f config/rbac/rbac.yaml
kubectl rollout restart deployment pod-sequence-controller -n pod-sequence-system
```

## Step 6: Test with Webhook-Enabled Sample
```bash
kubectl apply -f config/samples/test-node-scoped-webhook.yaml
```

## How It Works

1. **Annotations**: Pods must have these annotations:
   - `podsequence.example.com/name`: Name of the PodSequence
   - `podsequence.example.com/group`: Group index (0, 1, 2...)

2. **Webhook Injection**: When a pod is created, the webhook:
   - Checks for the annotations
   - Fetches the PodSequence
   - Injects tolerations for subsequent groups
   - Example: Group 0 gets tolerations for groups 1 and 2

3. **Scoping**: Only namespaces with label `podsequence-enabled=true` are affected

## Troubleshooting

### Check webhook logs:
```bash
kubectl logs -n pod-sequence-system -l app=pod-sequence-controller | grep webhook
```

### Verify webhook configuration:
```bash
kubectl get mutatingwebhookconfiguration podsequence-mutating-webhook -o yaml
```

### Check certificate secret:
```bash
kubectl get secret webhook-server-cert -n pod-sequence-system
```

### Test without webhook:
Remove namespace label: `kubectl label namespace default podsequence-enabled-`

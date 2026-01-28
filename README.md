# Pod Sequence Controller

A Kubernetes native controller that leverages the [Pod Scheduling Readiness](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-scheduling-readiness/) feature to create sequences for when pods become ready.

## Overview

The Pod Sequence Controller allows you to define ordered **pod groups** where scheduling gates are managed based on readiness at either the **cluster level** or **node level**.

### Cluster-Scoped Sequencing (Default)
Multiple pods in a tier start together across the entire cluster, with the next tier waiting for ALL pods in the current tier to be ready cluster-wide.

### Node-Scoped Sequencing
Pods are sequenced independently on each node. On each node, the next tier waits for ALL pods in the current tier on **that specific node** to be ready.

This is useful for scenarios such as:

**Cluster-Scoped:**
- Database initialization workflows with replicas
- Multi-tier application startup sequences (database → application → frontend)
- Ordered data processing pipelines
- Stateful application dependencies with high availability

**Node-Scoped:**
- CSI driver DaemonSets must be ready before pods with CSI volumes
- Device plugins (GPU, FPGA) must be ready before workloads using those devices
- Node monitoring agents before application pods
- Any node-level dependency (single-pod groups work too!)

## How It Works

1. **Create Pods with Scheduling Gates**: Define your pods with a scheduling gate (e.g., `podsequence.example.com/sequence-gate`)
2. **Define Pod Groups**: Organize your pods into logical groups (tiers) in a `PodSequence` resource
3. **Choose Scope**: Specify `scope: Cluster` (default) or `scope: Node`
4. **Controller manages sequencing**:
   - **Cluster scope**: All pods in a group have gates removed together; next group waits for all to be ready cluster-wide
   - **Node scope**: Pods in a group have gates removed per-node; next group on each node waits for all to be ready on that node

### Cluster-Scoped Example
- **Group 1** (Database): pod-1, pod-2 → Both start immediately
- **Group 2** (Application): pod-3, pod-4 → Start only after pod-1 **AND** pod-2 are ready
- **Group 3** (Frontend): pod-5 → Starts only after pod-3 **AND** pod-4 are ready

### Node-Scoped Example (CSI Driver)
- **Group 1** (CSI Driver): csi-node-worker1, csi-node-worker2 → Start immediately on their respective nodes
- **Group 2** (Apps): app-on-worker1, app-on-worker2 → app-on-worker1 starts only after csi-node-worker1 is ready; app-on-worker2 starts only after csi-node-worker2 is ready

**Note**: For sequential single-pod startup, simply create groups with one pod each!

### Pod Selection: Explicit Names vs. Label Selectors

Each `PodGroup` can specify pods in two ways:

#### Explicit Pod Names (`pods`)
Use when you know the exact pod names in advance:
```yaml
podGroups:
  - name: "Database"
    pods:
      - postgres-primary
      - postgres-replica-1
```
**Best for**: Individually named pods, StatefulSets with predictable names (e.g., `postgres-0`, `postgres-1`)

#### Label Selectors (`podSelector`)
Use for pods created by controllers with generated names:
```yaml
podGroups:
  - name: "CSI Driver"
    podSelector:
      matchLabels:
        app: csi-driver
        component: node
```
**Best for**: DaemonSets, Deployments, or any pods where names are auto-generated

**Rules**:
- You must specify **either** `pods` **or** `podSelector` (not both)
- Label selectors match pods in the same namespace as the `PodSequence`
- Use `matchLabels` for simple equality-based matching
- Use `matchExpressions` for more complex selection criteria

## Installation

### Prerequisites

- Kubernetes cluster version 1.27+ (Pod Scheduling Readiness is GA in 1.30+)
- kubectl configured to access your cluster
- Go 1.21+ (for building from source)

### Choose Your Deployment Mode

The controller offers two deployment modes:

#### 🚀 **Lite Mode** (Recommended for most users)
**Simple deployment with cluster-scoped sequencing only**

✅ No webhook required  
✅ No certificates needed  
✅ Simpler RBAC (no node permissions)  
✅ Perfect for application startup sequences  

```bash
# 1. Install the CRD
kubectl apply -f config/crd/podsequence-crd.yaml

# 2. Deploy the controller (lite mode)
kubectl apply -f config/rbac/rbac-lite.yaml
```

**Supports:** `scope: Cluster` only  
**Use for:** Database → App → Frontend sequences, StatefulSet ordering, pipeline stages

📚 [Lite Mode Documentation](config/rbac/README-LITE.md)

---

#### ⚡ **Full Mode** (Advanced)
**Complete deployment with node-scoped sequencing**

Includes all Lite Mode features, plus:  
✅ Per-node sequencing with taints  
✅ Webhook for automatic toleration injection  
✅ Support for DaemonSet dependencies  

Requires:
- Certificate generation (OpenSSL)
- Webhook configuration
- Node management permissions

```bash
# 1. Install the CRD
kubectl apply -f config/crd/podsequence-crd.yaml

# 2. Generate webhook certificates
chmod +x config/webhook/generate-certs.sh
./config/webhook/generate-certs.sh

# 3. Deploy webhook configuration
kubectl apply -f config/webhook/webhook.yaml

# 4. Patch webhook with CA bundle (command from step 2)
kubectl patch mutatingwebhookconfiguration podsequence-mutating-webhook \
  --type='json' -p='[{"op": "replace", "path": "/webhooks/0/clientConfig/caBundle", "value":"<CA_BUNDLE>"}]'

# 5. Deploy the controller (full mode)
kubectl apply -f config/rbac/rbac.yaml
```

**Supports:** Both `scope: Cluster` and `scope: Node`  
**Use for:** CSI drivers, device plugins, node-level dependencies, DaemonSets

📚 [Webhook Setup Guide](config/webhook/README.md)

---

### Which Mode Do I Need?

| You Need | Mode |
|----------|------|
| Wait for database before app | **Lite** |
| Sequential StatefulSet startup | **Lite** |
| Multi-tier application dependencies | **Lite** |
| Pipeline/workflow stages | **Lite** |
| CSI driver before storage pods | **Full** |
| Device plugin before GPU workloads | **Full** |
| Per-node DaemonSet dependencies | **Full** |

**Default recommendation: Start with Lite Mode.** Upgrade to Full Mode later if you need node-scoped sequencing.

### Build Custom Image (Optional)
### Build Custom Image (Optional)

If you want to build from source:

```bash
# Build the image
make docker-build

# For kind clusters
kind load docker-image pod-sequence-controller:latest

# For minikube
minikube image load pod-sequence-controller:latest
```

## Usage

### Basic Example

Create three pods with scheduling gates and a PodSequence with pod groups:

```yaml
apiVersion: scheduling.example.com/v1alpha1
kind: PodSequence
metadata:
  name: example-sequence
  namespace: default
spec:
  podGroups:
    - name: "Pod 1"
      pods:
        - pod-1
    - name: "Pod 2"
      pods:
        - pod-2
    - name: "Pod 3"
      pods:
        - pod-3
---
apiVersion: v1
kind: Pod
metadata:
  name: pod-1
  namespace: default
spec:
  schedulingGates:
  - name: podsequence.example.com/sequence-gate
  containers:
  - name: nginx
    image: nginx:latest
    readinessProbe:
      httpGet:
        path: /
        port: 80
      initialDelaySeconds: 5
      periodSeconds: 5
---
apiVersion: v1
kind: Pod
metadata:
  name: pod-2
  namespace: default
spec:
  schedulingGates:
  - name: podsequence.example.com/sequence-gate
  containers:
  - name: nginx
    image: nginx:latest
    readinessProbe:
      httpGet:
        path: /
        port: 80
      initialDelaySeconds: 5
      periodSeconds: 5
---
apiVersion: v1
kind: Pod
metadata:
  name: pod-3
  namespace: default
spec:
  schedulingGates:
  - name: podsequence.example.com/sequence-gate
  containers:
  - name: nginx
    image: nginx:latest
    readinessProbe:
      httpGet:
        path: /
        port: 80
      initialDelaySeconds: 5
      periodSeconds: 5
```

Apply the example:

```bash
kubectl apply -f config/samples/example-podsequence.yaml
```

### DaemonSet Example with Label Selector

For DaemonSets (which generate pod names), use label selectors:

```yaml
apiVersion: scheduling.example.com/v1alpha1
kind: PodSequence
metadata:
  name: daemonset-sequence
  namespace: default
spec:
  scope: Node  # Node-scoped for node-level dependencies
  podGroups:
    - name: "Device Plugin"
      podSelector:
        matchLabels:
          app: gpu-plugin
    - name: "GPU Workload"
      podSelector:
        matchLabels:
          app: ml-training
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: gpu-plugin
  namespace: default
spec:
  selector:
    matchLabels:
      app: gpu-plugin
  template:
    metadata:
      labels:
        app: gpu-plugin
    spec:
      schedulingGates:
      - name: podsequence.example.com/sequence-gate
      containers:
      - name: plugin
        image: nvidia/k8s-device-plugin:v0.14.0
```

### Monitor the Sequence

Watch the PodSequence status:

```bash
kubectl get podseq example-sequence -w
```

Check pod status:

```bash
kubectl get pods -w
```

You'll see pods transition through:
1. `SchedulingGated` - Waiting for scheduling gate to be removed
2. `Pending` - Scheduling gate removed, being scheduled
3. `Running` - Pod is running and becoming ready

### Cluster-Scoped Pod Groups Example

Use pod groups to manage multi-tier applications with high availability:

```yaml
apiVersion: scheduling.example.com/v1alpha1
kind: PodSequence
metadata:
  name: podgroups-example
  namespace: default
spec:
  scope: Cluster  # Default - cluster-wide sequencing
  podGroups:
    - name: "Database Layer"
      pods:
        - pod-1  # DB primary
        - pod-2  # DB replica
    - name: "Application Layer"
      pods:
        - pod-3  # App server 1
        - pod-4  # App server 2
    - name: "Frontend Layer"
      pods:
        - pod-5  # Frontend
```

**Behavior**:
1. `pod-1` and `pod-2` start immediately (Database Layer)
2. Once **both** `pod-1` and `pod-2` are ready, `pod-3` and `pod-4` start together (Application Layer)
3. Once **both** `pod-3` and `pod-4` are ready, `pod-5` starts (Frontend Layer)

See `config/samples/podgroups-example.yaml` for the complete example.

### Database Initialization Example

A sequential single-pod example using pod groups:

```yaml
apiVersion: scheduling.example.com/v1alpha1
kind: PodSequence
metadata:
  name: database-init-sequence
  namespace: default
spec:
  podGroups:
    - name: "Initialize Schema"
      pods:
        - init-db
    - name: "Start Database"
      pods:
        - database
    - name: "Run Migrations"
      pods:
        - migrate
    - name: "Start Application"
      pods:
        - app
  schedulingGateName: podsequence.example.com/db-init-gate
```

See `config/samples/database-init-example.yaml` for the complete example.

### Node-Scoped CSI Driver Example

Ensure CSI driver DaemonSet pods are ready before application pods that need CSI volumes:

```yaml
apiVersion: scheduling.example.com/v1alpha1
kind: PodSequence
metadata:
  name: csi-driver-sequence
  namespace: default
spec:
  scope: Node  # Node-level sequencing
  podGroups:
    - name: "CSI Driver"
      podSelector:
        matchLabels:
          app: csi-driver
          component: node
    - name: "Application Pods"
      podSelector:
        matchLabels:
          app: my-app
```

**Behavior**:
- On each node: application pods wait for CSI driver pods to be ready on that same node
- **Each node progresses independently**
- **Uses label selectors** to support DaemonSets with generated pod names

See `config/samples/node-scoped-csi-example.yaml` and `config/samples/node-scoped-device-plugin-example.yaml` for complete examples.

## PodSequence API

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `podGroups` | `[]PodGroup` | Yes | Ordered list of pod groups |
| `scope` | `string` | No | `Cluster` (default) or `Node` - determines sequencing scope |
| `namespace` | `string` | No | Namespace where pods are located (defaults to PodSequence namespace) |
| `schedulingGateName` | `string` | No | Name of the scheduling gate to manage (default: `podsequence.example.com/sequence-gate`) |

#### PodGroup Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | No | Optional name for the group (for identification) |
| `pods` | `[]string` | Conditional | List of explicit pod names in this group. Either `pods` or `podSelector` must be specified. |
| `podSelector` | `LabelSelector` | Conditional | Label selector to match pods in this group. Either `pods` or `podSelector` must be specified. Use this for DaemonSets or Deployments. |
| `nodeSelector` | `map[string]string` | No | Node selector for node-scoped sequences (future use) |

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `phase` | `string` | Current phase: `Pending`, `InProgress`, `Completed`, or `Failed` |
| `currentIndex` | `int` | Index currently being processed (cluster-wide or min across nodes) |
| `nodeStatus` | `[]NodeSequenceStatus` | Per-node status for node-scoped sequences |
| `currentGroupPods` | `[]string` | Pod names in the current group (cluster-scoped only) |
| `readyPodsInCurrentGroup` | `int` | Count of ready pods in current group (cluster-scoped only) |
| `processedPods` | `[]string` | List of pods that have been successfully processed |
| `message` | `string` | Human-readable status message |

#### NodeSequenceStatus Fields (Node-scoped only)

| Field | Type | Description |
|-------|------|-------------|
| `nodeName` | `string` | Name of the node |
| `currentIndex` | `int` | Current group index on this node |
| `readyPodsInCurrentGroup` | `int` | Ready pods in current group on this node |
| `phase` | `string` | Phase for this node |
| `conditions` | `[]Condition` | Standard Kubernetes conditions |

## Development

### Build

```bash
make build
```

### Run locally (outside cluster)

```bash
make run
```

### Run tests

```bash
make test
```

### Build Docker image

```bash
make docker-build IMG=your-registry/pod-sequence-controller:tag
```

### Deploy to cluster

```bash
make deploy
make docker-build
# Load image into your cluster (kind/minikube)
make deploy-example
```

## Architecture

The controller uses the controller-runtime framework and implements a reconciliation loop that:

1. Watches `PodSequence` resources
2. For each sequence, processes pods in order
3. Checks if the current pod in sequence is ready
4. If ready, removes the scheduling gate from the next pod
5. Continues until all pods in the sequence are ready

### Key Components

- **Custom Resource Definition (CRD)**: Defines the `PodSequence` API
- **Controller**: Reconciliation logic for managing pod scheduling gates
- **RBAC**: Permissions to watch/update pods and PodSequence resources

## Troubleshooting

### Pod stuck in SchedulingGated state

Check the PodSequence status:
```bash
kubectl describe podseq <name>
```

Check if previous pod is ready:
```bash
kubectl get pods
kubectl describe pod <previous-pod-name>
```

### Controller not removing gates

Check controller logs:
```bash
kubectl logs -n pod-sequence-system deployment/pod-sequence-controller
```

Check RBAC permissions:
```bash
kubectl auth can-i update pods --as=system:serviceaccount:pod-sequence-system:pod-sequence-controller
```

### PodSequence shows Failed status

Check the status message:
```bash
kubectl get podseq <name> -o yaml
```

Common causes:
- Pod not found in the specified namespace
- Pod doesn't have the expected scheduling gate
- Previous pod failed to become ready

## Cleanup

Remove examples:
```bash
kubectl delete -f config/samples/example-podsequence.yaml
```

Uninstall controller:
```bash
make undeploy
```

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the Apache License 2.0.

## References

- [Kubernetes Pod Scheduling Readiness](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-scheduling-readiness/)
- [Kubebuilder Documentation](https://book.kubebuilder.io/)
- [Controller Runtime](https://github.com/kubernetes-sigs/controller-runtime)

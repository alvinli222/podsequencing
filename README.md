# Pod Sequence Controller

A Kubernetes native controller that leverages the [Pod Scheduling Readiness](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-scheduling-readiness/) feature to create sequences for when pods become ready.

## Overview

The Pod Sequence Controller allows you to define an ordered sequence of pods or **pod groups** where scheduling gates are managed based on readiness. This enables:

- **Sequential Pod Startup**: Each pod waits for the previous pod to be ready
- **Pod Groups**: Multiple pods in a tier that start together, with the next tier waiting for ALL pods in the current tier to be ready

This is useful for scenarios such as:

- Database initialization workflows with replicas
- Multi-tier application startup sequences (database → application → frontend)
- Ordered data processing pipelines
- Stateful application dependencies with high availability

## How It Works

### Single Pod Sequence (Legacy Mode)

1. **Create Pods with Scheduling Gates**: Define your pods with a scheduling gate (e.g., `podsequence.example.com/sequence-gate`)
2. **Define a PodSequence Resource**: Create a `PodSequence` custom resource that specifies the order in which pods should become ready
3. **Controller Manages the Sequence**: The controller monitors pod readiness and removes scheduling gates in order:
   - Pod 1's scheduling gate is removed immediately
   - Once Pod 1 is ready, Pod 2's scheduling gate is removed
   - Once Pod 2 is ready, Pod 3's scheduling gate is removed
   - And so on...

### Pod Groups (Recommended)

1. **Define Pod Groups**: Organize your pods into logical groups (tiers)
2. **All pods in a group start together**: When a group's turn comes, all its pods have their scheduling gates removed simultaneously
3. **Next group waits for ALL pods**: The next group only starts when **all pods** in the current group are ready

**Example**: 
- **Group 1** (Database): pod-1, pod-2 → Both start immediately
- **Group 2** (Application): pod-3, pod-4 → Start only after pod-1 **AND** pod-2 are ready
- **Group 3** (Frontend): pod-5 → Starts only after pod-3 **AND** pod-4 are ready

## Installation

### Prerequisites

- Kubernetes cluster version 1.27+ (Pod Scheduling Readiness is GA in 1.30+)
- kubectl configured to access your cluster
- Go 1.21+ (for building from source)

### Deploy the Controller

1. **Install the CRD**:
   ```bash
   kubectl apply -f config/crd/podsequence-crd.yaml
   ```

2. **Deploy the controller**:
   ```bash
   kubectl apply -f config/rbac/rbac.yaml
   ```

3. **Build and load the controller image** (if running locally):
   ```bash
   make docker-build
   # For kind clusters
   kind load docker-image pod-sequence-controller:latest
   # For minikube
   minikube image load pod-sequence-controller:latest
   ```

## Usage

### Basic Example

Create three pods with scheduling gates and a PodSequence to manage their order:

```yaml
apiVersion: scheduling.example.com/v1alpha1
kind: PodSequence
metadata:
  name: example-sequence
  namespace: default
spec:
  sequence:
    - pod-1
    - pod-2
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

### Pod Groups Example

Use pod groups to manage multi-tier applications with high availability:

```yaml
apiVersion: scheduling.example.com/v1alpha1
kind: PodSequence
metadata:
  name: podgroups-example
  namespace: default
spec:
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

A more realistic example showing database initialization sequence:

```yaml
apiVersion: scheduling.example.com/v1alpha1
kind: PodSequence
metadata:
  name: database-init-sequence
  namespace: default
spec:
  sequence:
    - init-db      # Initialize schema
    - database     # Start database
    - migrate      # Run migrations
    - app          # Start application
  schedulingGateName: podsequence.example.com/db-init-gate
```

See `config/samples/database-init-example.yaml` for the complete example.

## PodSequence API

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `podGroups` | `[]PodGroup` | No* | Ordered list of pod groups (recommended) |
| `sequence` | `[]string` | No* | Ordered list of pod names (legacy mode) |
| `namespace` | `string` | No | Namespace where pods are located (defaults to PodSequence namespace) |
| `schedulingGateName` | `string` | No | Name of the scheduling gate to manage (default: `podsequence.example.com/sequence-gate`) |

*Either `podGroups` or `sequence` must be specified. If both are provided, `podGroups` takes precedence.

#### PodGroup Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | No | Optional name for the group (for identification) |
| `pods` | `[]string` | Yes | List of pod names in this group |

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `phase` | `string` | Current phase: `Pending`, `InProgress`, `Completed`, or `Failed` |
| `currentIndex` | `int` | Index in the sequence/group currently being processed |
| `currentGroupPods` | `[]string` | Pod names in the current group (pod groups only) |
| `readyPodsInCurrentGroup` | `int` | Count of ready pods in current group (pod groups only) |
| `processedPods` | `[]string` | List of pods that have been successfully processed |
| `message` | `string` | Human-readable status message |
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

# Pod Sequence Controller

Control **when Kubernetes pods become eligible for scheduling**, using ordered groups of pods. For example, let database pods become Ready before scheduling application pods, or wait for a node's infrastructure agent before placing workloads on that node.

The controller uses [Pod Scheduling Readiness](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-scheduling-readiness/). It removes scheduling gates when dependencies are ready; Kubernetes still handles placement, resources, and starting containers.

## Contents

- [Choose a scope](#choose-a-scope)
- [Install](#install)
- [Cluster-scoped PodGroups](#cluster-scoped-podgroups)
- [Node-scoped pods](#node-scoped-pods)
- [Configuration reference](#configuration-reference)
- [Monitoring and troubleshooting](#monitoring-and-troubleshooting)
- [Cleanup](#cleanup)
- [Development](#development)

## Choose a scope

| | Cluster scope | Node scope |
|---|---|---|
| Dependency boundary | All selected pods across nodes, in one target namespace | Selected pods on each participating node |
| Typical use | Database, then application, then frontend | Node agent, then workloads that need it |
| Progress | The next group waits for the current group | Each node progresses independently |
| Pod selection | Explicit names or label selectors | Label selectors only |
| Lifecycle | One-time startup sequence; ends at `Completed` | Continuous evaluation; `Active` when all participating nodes are satisfied |
| Installation | Lite or Full | Full |

**"Cluster scope" does not mean a cluster-scoped Kubernetes resource.** `PodSequence` is namespaced in both modes. Each sequence manages pods in exactly one namespace: its own by default, or `spec.namespace`. It cannot express a dependency across multiple pod namespaces.

A **PodGroup** is an entry in `spec.podGroups`, not a separate resource. Groups are ordered; pods within a group are released without an ordering between them. Gate removal does not guarantee simultaneous scheduling or startup.

## Install

### Prerequisites

- Kubernetes 1.27+ with Pod Scheduling Readiness available; Kubernetes 1.30+ is recommended because the feature is stable there.
- `kubectl` configured for the intended cluster, with permission to install CRDs and cluster RBAC.
- A controller image your cluster can pull. The supplied manifests use `podsequencing.azurecr.io/pod-sequence-controller:latest`; replace it if needed.
- For Full mode: Bash and OpenSSL to run the certificate helper.
- For source builds: Go 1.21+; Docker for container builds.

Commands below use Bash syntax and run from the repository root. On Windows, use WSL or a suitable Bash environment for the shell script and Make targets.

### Lite mode: cluster-scoped sequences

Start here if you only need application startup ordering. There is no webhook, certificate setup, or node access. You must put scheduling gates in your pod manifests or workload pod templates.

```bash
kubectl apply -f config/crd/podsequence-crd.yaml
kubectl apply -f config/rbac/rbac-lite.yaml

# If using your own image, replace the image before waiting for the rollout:
# kubectl -n pod-sequence-system set image deployment/pod-sequence-controller manager=YOUR_REGISTRY/pod-sequence-controller:TAG

kubectl -n pod-sequence-system rollout status deployment/pod-sequence-controller
```

See the [Lite mode guide](config/rbac/README-LITE.md) for additional deployment details.

### Full mode: node scope and automatic gate injection

Full mode supports both scopes. It enables the webhooks and grants the controller access to Nodes and DaemonSets.

1. Install the CRD and controller resources. The controller may wait for the certificate Secret until the next step.

   ```bash
   kubectl apply -f config/crd/podsequence-crd.yaml
   kubectl apply -f config/rbac/rbac.yaml
   ```

2. Generate the certificate Secret, then install the webhook configuration.

   ```bash
   bash config/webhook/generate-certs.sh
   kubectl apply -f config/webhook/webhook.yaml
   ```

3. Run the `kubectl patch mutatingwebhookconfiguration ...` command printed by the certificate helper. It sets the CA bundle for **both** webhooks.

4. Set your controller image if necessary, then wait for the deployment before creating workloads.

   ```bash
   # kubectl -n pod-sequence-system set image deployment/pod-sequence-controller manager=YOUR_REGISTRY/pod-sequence-controller:TAG
   kubectl -n pod-sequence-system rollout status deployment/pod-sequence-controller
   ```

See the [webhook setup guide](config/webhook/README.md) for certificate and admission details. `make deploy` alone does not perform certificate or webhook setup.

**Create the PodSequence before its workloads.** Automatic gate injection requires an existing sequence, a matching group, and this label on each pod:

```yaml
metadata:
  labels:
    podsequence.example.com/sequence: my-sequence
```

For Deployments and StatefulSets, put the label in `spec.template.metadata.labels`. For DaemonSets, put it on **both the DaemonSet metadata and its pod template** so both webhooks receive the relevant objects.

The label opts a pod into admission mutation; the group's `pods` or `podSelector` determines membership. A label alone is not a group selector.

> The supplied webhooks use `failurePolicy: Ignore`. If admission is unavailable, pods can be created without gates and start unordered. A missing sequence or unmatched group also leaves a pod ungated. Gates cannot be added after pod creation; adding a label to an existing pod is not enough. The webhook configuration excludes the `pod-sequence-system` namespace.

## Cluster-scoped PodGroups

### Example 1: database, application, frontend

This sequence has three groups and five named pods:

```yaml
apiVersion: scheduling.example.com/v1alpha1
kind: PodSequence
metadata:
  name: podgroups-example
  namespace: default
spec:
  scope: Cluster
  podGroups:
    - name: Database Layer
      pods: [pod-1, pod-2]
    - name: Application Layer
      pods: [pod-3, pod-4]
    - name: Frontend Layer
      pods: [pod-5]
```

| Step | What happens |
|---|---|
| Database | Gates are removed from `pod-1` and `pod-2`. Both must exist and become Ready. |
| Application | Gates are removed from `pod-3` and `pod-4` after the database group passes. Both must become Ready. |
| Frontend | The gate is removed from `pod-5` after the application group passes. |
| Completion | When the frontend group passes, the sequence becomes `Completed`. |

Apply the [complete example](config/samples/podgroups-example.yaml), which includes the sequence, all five pods, scheduling gates, and readiness probes:

```bash
kubectl apply -f config/samples/podgroups-example.yaml
kubectl get podseq podgroups-example -n default -w
```

In a second terminal:

```bash
kubectl get pods -n default -w
```

The two PostgreSQL pods in this demo illustrate a group; the manifest does not configure database replication.

For your own named pods in Lite mode, include a gate at creation time, as in this single-pod manifest:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: pod-1
  namespace: default
spec:
  schedulingGates:
    - name: podsequence.example.com/sequence-gate
  containers:
    - name: demo
      image: nginx:stable
      readinessProbe:
        httpGet:
          path: /
          port: 80
        periodSeconds: 5
```

This is an alternative demonstration pod, not an additional resource to apply over the PostgreSQL example. Add the same gate to every pod you want ordered, including the first group.

### Example 2: select generated pod names and add a readiness delay

Use label selectors for pods created by Deployments, StatefulSets, or other workload controllers:

```yaml
apiVersion: scheduling.example.com/v1alpha1
kind: PodSequence
metadata:
  name: application-startup
  namespace: default
spec:
  scope: Cluster
  podGroups:
    - name: Database
      podSelector:
        matchLabels:
          app: shop
          tier: database
      waitDuration: 30s
    - name: API
      podSelector:
        matchLabels:
          app: shop
          tier: api
    - name: Web
      podSelector:
        matchLabels:
          app: shop
          tier: web
```

This defines the sequence only; create the corresponding workloads separately. For example, the API Deployment would contain this **pod-template fragment** in Lite mode:

```yaml
spec:
  template:
    metadata:
      labels:
        app: shop
        tier: api
    spec:
      schedulingGates:
        - name: podsequence.example.com/sequence-gate
```

Repeat with `tier: database` and `tier: web` for the other workloads, and supply their containers and readiness probes. In Full mode, you can replace the explicit gate with the template label `podsequence.example.com/sequence: application-startup`.

The database group passes once every currently selected pod is Ready and has remained Ready for at least 30 seconds. `waitDuration` uses each pod's Ready-condition transition time, not the time the controller first notices readiness.

### Cluster-scope rules to know

- Each group must use exactly one of a nonempty `pods` list or `podSelector`. Selectors support `matchLabels` and `matchExpressions`. Keep groups and sequences non-overlapping.
- Named groups wait for every listed pod to exist. Selector groups wait for at least one matching pod, then evaluate the matching pods that currently exist; they do **not** wait for a Deployment's desired replica count or future replicas.
- Readiness means the pod's `Ready=True` condition, not just `Running`. Use probes that reflect the dependency you actually need.
- A one-pod group gives simple sequential startup. See the [three-pod example](config/samples/example-podsequence.yaml) and [wait-duration example](config/samples/example-with-waits.yaml).
- Cluster scope is a startup sequence, not continuous dependency enforcement. Recreated pods in already-passed groups are released; after `Completed`, newly created matching pods are also released without replaying the sequence.
- This is not a Job-completion workflow engine. A `Succeeded` pod is not a substitute for a Ready pod, so do not use job completion as a readiness dependency.

## Node-scoped pods

Node scope is for dependencies that must be ready **on the same node** as their consumers. It requires Full mode and `podSelector` on every group.

For example, if a node agent is Ready on `worker-a` but not `worker-b`, dependent pods can proceed on `worker-a` without waiting for `worker-b`. A node-scoped sequence does not require one application replica per node or distribute replicas evenly.

### Example: node agent before application pods

The following demo uses an NGINX DaemonSet as a stand-in for a node agent. It requires no GPU or storage driver.

First, save and apply this PodSequence:

```yaml
apiVersion: scheduling.example.com/v1alpha1
kind: PodSequence
metadata:
  name: node-startup
  namespace: default
spec:
  scope: Node
  nodeSelector:
    matchLabels:
      kubernetes.io/os: linux
  nodeEligibility:
    method: NodeLabel
  gateTimeout: 10m
  podGroups:
    - name: Node agent
      podSelector:
        matchLabels:
          app: demo-node-agent
      policy: RequireOnNode
      waitDuration: 10s
    - name: Application
      podSelector:
        matchLabels:
          app: demo-app
```

Then save and apply these workloads:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: demo-node-agent
  namespace: default
  labels:
    podsequence.example.com/sequence: node-startup
spec:
  selector:
    matchLabels:
      app: demo-node-agent
  template:
    metadata:
      labels:
        app: demo-node-agent
        podsequence.example.com/sequence: node-startup
    spec:
      nodeSelector:
        kubernetes.io/os: linux
      containers:
        - name: agent
          image: nginx:stable
          readinessProbe:
            httpGet:
              path: /
              port: 80
            periodSeconds: 5
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo-app
  namespace: default
spec:
  replicas: 2
  selector:
    matchLabels:
      app: demo-app
  template:
    metadata:
      labels:
        app: demo-app
        podsequence.example.com/sequence: node-startup
    spec:
      containers:
        - name: app
          image: nginx:stable
          readinessProbe:
            httpGet:
              path: /
              port: 80
            periodSeconds: 5
```

The webhook adds gates to both kinds of pod. The controller releases each agent immediately because it is in the first group. Application pods become eligible for nodes whose agent has been Ready for 10 seconds.

`RequireOnNode` deliberately keeps a participating node unsatisfied if no agent can run there, for example because of an untolerated control-plane taint. Restrict the sequence's node selector to your intended worker pool, or configure the agent's placement and tolerations accordingly.

### Pinned pods: the target node is already known

DaemonSet pods have required node affinity targeting a specific node's `metadata.name` before they are scheduled. The controller can therefore keep a later-group DaemonSet pod gated until all earlier groups on **that node** are satisfied.

For an agent, exporter, and application sequence:

| On each node | Release rule |
|---|---|
| Agent DaemonSet pod | No earlier group, so release immediately |
| Exporter DaemonSet pod | Wait for that node's agent group, including its readiness delay |
| Application pod | Only use nodes where both earlier groups are satisfied |

Here, "pinned" means the target can be identified before scheduling from single-node required affinity. Do not set `spec.nodeName` to implement sequencing: it bypasses the scheduler, cannot be combined with scheduling gates, and the webhook skips such pods.

### Unpinned pods: the scheduler still needs to choose a node

Deployment pods usually have no known target node. Holding their gate until "their node" is ready would be circular. Instead, the controller:

1. Counts the consecutive satisfied groups on each participating node.
2. Publishes that count as a node label or taint.
3. Adds the appropriate affinity or tolerations to the still-gated pod, then removes its gate.

**An application pod can therefore be `Pending` without a gate while it waits for an eligible node.** This is expected; the scheduler now enforces placement.

The per-sequence key is `podsequence.example.com/seq-<hash>`, derived from the PodSequence namespace and name. Read its exact value from `status.nodeEligibilityKey`.

| Method | How it works | Impact |
|---|---|---|
| `NodeLabel` (default) | Nodes carry the satisfied-group count. Later-group unpinned pods receive required node affinity for a high enough count. | Only constrained member pods are affected; unrelated workloads are unchanged. |
| `Taint` | Nodes carry the count in a `NoSchedule` taint until all groups are satisfied. Unpinned members receive tolerations for counts at which they may proceed. | Also blocks unrelated pods that do not tolerate the taint. |

In `NodeLabel` mode, a second-group pod requires a count greater than `0`; a third-group pod requires a count greater than `1`. Existing required affinity remains in effect. Later-group unpinned members are restricted to participating nodes, but **first-group pods receive no additional affinity**. Apply normal workload placement constraints when you need to limit those pods too.

In `Taint` mode, first-group unpinned pods tolerate all values of the sequence taint. Later groups tolerate only sufficiently advanced values. Non-participating nodes are untainted, so pods may also schedule there unless their own placement constraints prevent it. Broad pre-existing tolerations can defeat this restriction.

DaemonSets need the sequence-taint toleration in their **pod template**, not merely on their generated pods: the DaemonSet controller checks whether it can create a pod on the node first. The DaemonSet webhook adds this toleration to opted-in DaemonSets. Missing tolerations appear in the sequence's `Degraded` condition.

### When is a group satisfied on a node?

A group is satisfied when all its active pods targeting that node are Ready for the configured `waitDuration`, and each matching DaemonSet expected on that node has a pod there. Terminating, `Succeeded`, and `Failed` pods are excluded from node-scope evaluation.

DaemonSet expectations account for template node selectors, required affinity, and taints/tolerations. The sequence's own taint and transient node-initialization taints do not make an expected DaemonSet optional. This prevents a newly joined node from passing a group just because its DaemonSet pod has not been created yet.

If no pod targets the node and no matching DaemonSet is expected there:

| Group policy | Result | Use when |
|---|---|---|
| `SkipIfNotScheduled` (default) | Treat the group as satisfied on that node | The group is optional there, such as an application that runs on only some nodes |
| `RequireOnNode` | Keep waiting for at least one Ready pod on that node | Every participating node must have this dependency |

`RequireOnNode` does not create or spread pods. A Deployment with fewer replicas than participating nodes may never satisfy that policy everywhere. Also create dependency DaemonSets before dependent workloads, or use `RequireOnNode` where absence must block: the controller cannot infer a DaemonSet that does not exist yet.

### Continuous enforcement, timeouts, and limits

Node scope keeps evaluating new nodes, replacement pods, and DaemonSet rollouts. It reports `Active` when all participating nodes are satisfied and returns to `InProgress` when dependencies are missing or unready. It never finishes as `Completed`.

| Control | Behavior |
|---|---|
| `spec.nodeSelector` | Selects participating nodes; defaults to all nodes. Pinned pods targeting excluded or missing nodes have their gates released. |
| `spec.gateTimeout` | Measures a blocked pinned pod's age from creation. On expiry, marks its node `Failed`, sets `Degraded=True`, and emits a Warning event. It does **not** release the gate. |
| `podsequence.example.com/bypass: "true"` | Pod annotation for skipping admission gating or releasing a still-gated node-scope member without adding eligibility constraints. Does not remove affinity already added to an ungated pod. |

The timeout does not cover unpinned Deployment pods waiting on scheduler affinity, nor an ungated first-group pod failing to become Ready.

**This controls scheduling, not runtime dependency health.** Already-bound pods are not stopped or re-gated if a dependency fails. Kubelet restarts after a reboot do not replay scheduling order. Use application retries, init containers, and suitable startup/readiness probes for runtime dependencies.

In Taint mode, a new node can accept workloads before the controller applies its taint. If that gap is unacceptable, register nodes with the sequence's `NoSchedule` taint at value `0`. Neither taints nor affinity make this an atomic runtime dependency guarantee.

For infrastructure-specific patterns, see the [CSI example](config/samples/node-scoped-csi-example.yaml), [GPU plugin/exporter/workload example](config/samples/node-scoped-device-plugin-example.yaml), and [Taint-mode example](config/samples/node-scoped-taint-example.yaml). Adapt them to your actual drivers and node configuration; the CSI sample is illustrative, not a complete CSI installation. Native resource constraints such as `nvidia.com/gpu` should still be used where applicable.

## Configuration reference

API: `scheduling.example.com/v1alpha1`, kind: `PodSequence`, short name: `podseq`.

### Sequence fields

| Field under `spec` | Default | Meaning |
|---|---|---|
| `podGroups` | Required | Nonempty ordered list of groups |
| `scope` | `Cluster` | `Cluster` or `Node`; immutable after creation |
| `namespace` | Resource namespace | The single namespace containing managed pods |
| `schedulingGateName` | `podsequence.example.com/sequence-gate` | Gate to manage; explicit pod gates must match |
| `nodeSelector` | All nodes | Label selector for participating nodes in Node scope |
| `nodeEligibility.method` | `NodeLabel` | `NodeLabel` or `Taint`, for Node scope |
| `gateTimeout` | No timeout | Duration such as `10m`, for blocked pinned pods in Node scope |

### Group fields

| Field under `spec.podGroups[]` | Default | Meaning |
|---|---|---|
| `name` | Generated display name | Human-readable group name |
| `pods` | Unset | Explicit pod names; Cluster scope only |
| `podSelector` | Unset | Kubernetes label selector; required in Node scope |
| `waitDuration` | No extra wait | Time each pod must remain Ready, such as `30s` or `1m30s`; evaluated per node in Node scope |
| `policy` | `SkipIfNotScheduled` | Node-scope handling of absent groups: `SkipIfNotScheduled` or `RequireOnNode` |

Exactly one of a nonempty `pods` list or `podSelector` is required per group. See the [API types](api/v1alpha1/podsequence_types.go) and [CRD](config/crd/podsequence-crd.yaml) for the schema.

## Monitoring and troubleshooting

Replace `SEQUENCE_NAME` and `POD_NAMESPACE` with your resource name and namespace:

```bash
kubectl get podseq SEQUENCE_NAME -n POD_NAMESPACE -o wide
kubectl describe podseq SEQUENCE_NAME -n POD_NAMESPACE
kubectl get podseq SEQUENCE_NAME -n POD_NAMESPACE -o yaml
kubectl get pods -n POD_NAMESPACE -o wide
kubectl logs -n pod-sequence-system deployment/pod-sequence-controller
```

If `spec.namespace` points elsewhere, inspect the sequence in its resource namespace and the pods in the target namespace.

### Read the status

| Field | What it tells you |
|---|---|
| `phase`, `message` | Overall progress and the current reason for waiting |
| `currentIndex` | Zero-based current group; in Node scope, the minimum satisfied-group count across participating nodes |
| `currentGroupPods`, `readyPodsInCurrentGroup`, `processedPods` | Cluster-scope pod progress |
| `summary` | Node-scope totals: `totalNodes`, `satisfiedNodes`, `failedNodes`, `gatedPods` |
| `nodeStatus` | Unsatisfied nodes and their waiting reasons; failed nodes first, capped at 50 entries |
| `nodeEligibilityKey` | Node label or taint key for this sequence |
| `conditions` | Node-scope `Ready` and `Degraded` signals |

`Pending` means initialization; `InProgress` means work remains. Cluster scope ends at `Completed`; Node scope uses `Active` while all nodes are satisfied. A node timeout appears as `Failed` in `nodeStatus` and `Degraded=True`, not necessarily as an overall `Failed` phase.

### Common problems

| Symptom | What to check |
|---|---|
| Pod stays `SchedulingGated` | Earlier groups, Ready conditions, `waitDuration`, correct namespace, selector, and gate name |
| Pod is `Pending` without a gate | Scheduler events (`kubectl describe pod`), node eligibility, resources, volume constraints, and other taints/affinity |
| Pods start out of order | Gate existed at pod creation; webhook was healthy; sequence already existed; labels matched; sequence had not already completed |
| Selector group advances before every replica exists | Selectors evaluate existing pods, not desired replica counts; use explicit names when you need a known fixed set |
| New node never progresses | `nodeStatus.message`, expected DaemonSets, template placement constraints, and `RequireOnNode` policy |
| Taint mode reports `MissingToleration` | DaemonSet metadata opt-in label and template tolerations; update the DaemonSet after the sequence exists |
| Node sequence reports Full mode required | Deploy Full mode with `--enable-webhook`, certificates, webhooks, and node/DaemonSet RBAC |
| Gates are not removed | Controller logs and its permission to patch pods; also check for gates belonging to other controllers |

For a deliberate node-scope escape hatch:

```bash
kubectl annotate pod POD_NAME -n POD_NAMESPACE podsequence.example.com/bypass=true --overwrite
```

Bypassing one pod does not mark its group Ready or satisfy dependencies for later groups.

### Metrics

The controller exposes Prometheus metrics on port `8080` at `/metrics`:

| Metric | Meaning |
|---|---|
| `podsequence_nodes_total{namespace,name}` | Participating nodes |
| `podsequence_nodes_satisfied{namespace,name}` | Fully satisfied nodes |
| `podsequence_nodes_failed{namespace,name}` | Nodes exceeding `gateTimeout` |
| `podsequence_gated_pods{namespace,name}` | Node-scope member pods still gated, not all Pending pods |
| `podsequence_ungated_pods_total{namespace,name,reason}` | Gates removed, grouped by reason |

## Cleanup

Delete sequences **while the controller is still running**, so their finalizers can release managed gates and clean up node eligibility state:

```bash
kubectl delete podseq SEQUENCE_NAME -n POD_NAMESPACE
```

Deleting a sequence does not delete its workloads. Node taints are removed; eligibility labels still referenced by unscheduled pods are temporarily set to a permissive value and removed once no pending pod needs them.

Remove demo workloads with the same manifest files you applied. For the bundled cluster example:

```bash
kubectl delete -f config/samples/podgroups-example.yaml
```

Before uninstalling, remove every sequence and wait for deletion to finish. Stop or update workload templates with hard-coded gates, or future replacement pods can remain gated without a controller.

```bash
# Full mode only:
kubectl delete -f config/webhook/webhook.yaml
kubectl delete secret webhook-server-cert -n pod-sequence-system

# Delete the manifest for the mode you installed:
kubectl delete -f config/rbac/rbac.yaml
# For Lite mode, use config/rbac/rbac-lite.yaml instead.

# Removes the PodSequence API cluster-wide:
kubectl delete -f config/crd/podsequence-crd.yaml
```

The RBAC manifests also contain the `pod-sequence-system` Namespace; deleting one deletes that namespace and any remaining resources in it.

## Development

| Task | Command |
|---|---|
| Build the binary | `make build` |
| Run locally using kubeconfig, Cluster scope only | `make run` |
| Format, vet, and run tests | `make test` |
| Regenerate DeepCopy code and CRD after API changes | `make generate manifests` |
| Build an image | `make docker-build IMG=YOUR_REGISTRY/pod-sequence-controller:TAG` |
| Push that image | `make docker-push IMG=YOUR_REGISTRY/pod-sequence-controller:TAG` |

Use the installation steps above to deploy your image. For a local kind or minikube image, also adjust the Deployment's image reference and `imagePullPolicy`; the supplied manifests use `Always`.

Integration tests against a real API server run when `KUBEBUILDER_ASSETS` is set:

```bash
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.19
export KUBEBUILDER_ASSETS=$(setup-envtest use 1.30.x -p path)
go test ./internal/controller -run TestEnvtest -v
```

### Code layout

| Location | Responsibility |
|---|---|
| `api/v1alpha1` | PodSequence API and generated DeepCopy code |
| `internal/controller` | Reconciliation, per-node planning, status, metrics, and orphan cleanup |
| `internal/sequence` | Shared selection, readiness, affinity, and DaemonSet helpers |
| `internal/webhook` | Pod gate injection and DaemonSet template mutation |
| `config` | CRD, RBAC, webhook setup, and sample manifests |

The controller uses controller-runtime to watch PodSequences and Pods, plus Nodes and DaemonSets in Full mode. Node scope also performs a safety resync; orphan node labels and taints are cleaned up periodically.

Contributions are welcome through pull requests. This project is licensed under the Apache License 2.0.

### Further reading

- [Kubernetes Pod Scheduling Readiness](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-scheduling-readiness/)
- [Kubebuilder](https://book.kubebuilder.io/)
- [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime)

# Pod Sequence Controller

**Start pods in order: one group becomes Ready before the next group can schedule.**

Put pods into ordered groups in a `PodSequence`. The controller removes their [scheduling gates](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-scheduling-readiness/) when they can proceed. Kubernetes still decides placement and starts the containers.

| Scope | What waits for what? | Lifecycle |
|---|---|---|
| **Cluster** | The next group waits for all pods in the current group, across nodes. | One-time startup; ends at `Completed`. |
| **Node** | The next group waits for dependencies on the same node. Each node progresses independently. | Keeps evaluating new nodes and replacement pods; `Active` when satisfied. |

A group is an entry in `spec.podGroups`, not a separate resource. Both scopes manage pods in **one namespace**, not across namespaces. Ready means `Ready=True`, not merely Running.

## Install

Use Kubernetes 1.27+ (1.30+ recommended) and `kubectl` with cluster-admin installation permissions. Commands use Bash from the repository root.

| Mode | Supports | Setup |
|---|---|---|
| **Lite** | Cluster scope | Explicit gates in pod manifests; no webhook or certificates. |
| **Full** | Cluster and Node scope | Webhooks, certificates, and node permissions. |

### Lite

```bash
kubectl apply -f config/crd/podsequence-crd.yaml
kubectl apply -f config/rbac/rbac-lite.yaml
kubectl -n pod-sequence-system rollout status deployment/pod-sequence-controller
```

### Full

```bash
kubectl apply -f config/crd/podsequence-crd.yaml
kubectl apply -f config/rbac/rbac.yaml
bash config/webhook/generate-certs.sh
kubectl apply -f config/webhook/webhook.yaml
```

Run the **CA-bundle patch command printed by the certificate script**, then:

```bash
kubectl -n pod-sequence-system rollout status deployment/pod-sequence-controller
```

The manifests use `podsequencing.azurecr.io/pod-sequence-controller:latest`. If your cluster cannot pull it, set your own image before waiting for rollout. Full mode requires Bash and OpenSSL. See the [Lite guide](config/rbac/README-LITE.md) or [webhook setup guide](config/webhook/README.md) for details.

## 1. Cluster-level: database, application, frontend

**Database pods become Ready, then application pods, then the frontend.**

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

The [complete manifest](config/samples/podgroups-example.yaml) includes all five pods, gates, and readiness probes:

```bash
kubectl apply -f config/samples/podgroups-example.yaml
kubectl get podseq podgroups-example -n default -w
```

Pods within a group can start together; they are not ordered relative to each other. The two PostgreSQL pods illustrate a group, not configured database replication.

## How node-level sequencing works

There are two kinds of pod to handle:

| Pod type | How the controller holds it back |
|---|---|
| **DaemonSet pod: target node already known** | Keeps its scheduling gate until earlier groups on that node are Ready. |
| **Regular workload pod: scheduler chooses the node** | Removes its gate after adding placement rules, so it can only schedule where dependencies are ready. |

For regular workload pods, choose `spec.nodeEligibility.method`:

| Method | What it does | When to use it |
|---|---|---|
| **`NodeLabel` (default)** | Labels nodes with their progress; adds required node affinity to later-group pods. Unrelated workloads are unaffected. | Recommended for most sequences. Both demos below use it. |
| **`Taint`** | Applies a `NoSchedule` taint until all groups are satisfied; adds tolerations allowing member pods to proceed at the right stage. | When you also want to block unrelated workloads without tolerations. |

**DaemonSet-to-DaemonSet ordering uses gates on each node directly.** It does not need affinity to choose a node. With `NodeLabel`, the controller still records node progress, but adds no eligibility affinity to these already-targeted pods.

A group passes on a node when all its active pods there are Ready and every matching DaemonSet expected there has a pod. `waitDuration` adds a period of continuous readiness. If no pod or DaemonSet is expected, `SkipIfNotScheduled` skips the group; `RequireOnNode` waits for a pod instead.

### Before running either node demo

Install **Full mode**, then select one or more Linux worker nodes:

```bash
kubectl label node YOUR_WORKER_NODE podsequence-demo=true
```

Both demos use NGINX containers as stand-ins for agents, so no real driver installation is needed. Their manifests include explicit scheduling gates to avoid depending on webhook timing. Keep the controller running until you delete the demo workloads.

## 2. Node-level: node agent and regular workload pods

**A DaemonSet supplies the agent on each node. A Deployment supplies application pods that can use any ready node.**

```text
worker-a: agent Ready     -> application pods may schedule here
worker-b: agent not Ready -> application pods must wait or use worker-a
```

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
      podsequence-demo: "true"
      kubernetes.io/os: linux
  nodeEligibility:
    method: NodeLabel
  podGroups:
    - name: Node agent
      policy: RequireOnNode
      waitDuration: 10s
      podSelector:
        matchLabels:
          app: demo-node-agent
    - name: Application
      podSelector:
        matchLabels:
          app: demo-app
```

The agent starts immediately. After it has been Ready for 10 seconds on a node, that node becomes eligible for application pods. The Deployment's two replicas need not land on different nodes.

Apply the [complete manifest: sequence, agent DaemonSet, and application Deployment](config/samples/node-scoped-workload-example.yaml):

```bash
kubectl apply -f config/samples/node-scoped-workload-example.yaml
kubectl get pods -n default -l podsequence.example.com/sequence=node-startup -o wide -w
```

Application pods may show **Pending rather than SchedulingGated** while waiting: their gate has been removed, and node affinity now controls placement.

## 3. DaemonSet node-level: multiple DaemonSet groups

**Order whole groups of DaemonSets independently on every node. A slow node never holds up another node.**

```text
On EACH selected node:

[network agent + storage agent] -> [metrics agent] -> [node service]
        BOTH must be Ready          must be Ready
```

```yaml
apiVersion: scheduling.example.com/v1alpha1
kind: PodSequence
metadata:
  name: daemonset-startup
  namespace: default
spec:
  scope: Node
  nodeSelector:
    matchLabels:
      podsequence-demo: "true"
      kubernetes.io/os: linux
  podGroups:
    - name: Foundation agents
      policy: RequireOnNode
      podSelector:
        matchLabels:
          podsequence-demo-group: foundation
    - name: Metrics agent
      policy: RequireOnNode
      podSelector:
        matchLabels:
          podsequence-demo-group: metrics
    - name: Node service
      policy: RequireOnNode
      podSelector:
        matchLabels:
          podsequence-demo-group: service
```

| Group | DaemonSets | Release rule on each node |
|---|---|---|
| Foundation | `demo-network-agent`, `demo-storage-agent` | Both start immediately. |
| Metrics | `demo-metrics-agent` | Waits for **both** foundation pods on that node to be Ready. |
| Service | `demo-node-service` | Waits for the metrics pod on that node to be Ready. |

The [complete manifest](config/samples/node-scoped-daemonsets-example.yaml) creates all four DaemonSets with matching group labels, gates, and readiness probes:

```bash
kubectl apply -f config/samples/node-scoped-daemonsets-example.yaml
kubectl get pods -n default -l podsequence.example.com/sequence=daemonset-startup -o wide -w
```

Unlike the Deployment above, every DaemonSet pod already targets a particular node. Later groups stay **SchedulingGated** until that node's earlier groups pass. This ordering is evaluated again for replacement pods and newly participating nodes.

## Using your own workloads

| Setting | What to know |
|---|---|
| Group membership | Choose either explicit `pods` names or `podSelector`. Node scope requires selectors. Keep groups and sequences non-overlapping. |
| Scheduling gates | Add `podsequence.example.com/sequence-gate` at pod creation, or use Full mode's automatic injection. |
| Automatic injection | Create the sequence first. Set `podsequence.example.com/sequence: SEQUENCE_NAME` on pod templates and also on DaemonSet metadata. The pod must match a group. |
| Namespace | Defaults to the sequence's namespace; `spec.namespace` can target a different single namespace. |
| Participating nodes | `spec.nodeSelector` limits Node scope. Align it with your DaemonSets' placement rules. |
| Missing dependency | `RequireOnNode` blocks an empty group; it does not create pods or spread Deployment replicas. |
| Readiness delay | Group `waitDuration`, such as `10s`, requires continuous readiness before proceeding. |
| Timeout | Node-scope `gateTimeout`, such as `10m`, flags blocked pinned pods as failed without releasing them. It does not time out Deployment affinity waits. |

Full schema: [API types](api/v1alpha1/podsequence_types.go).

### Important limits

- **Scheduling only:** existing pods are not stopped if dependencies fail. Kubelet restarts do not replay the sequence. Use application retries and health probes too.
- **Cluster scope does not replay after completion.** Selector groups count existing pods, not a workload's desired replica count. Job completion is not pod readiness.
- **Webhook injection is fail-open by default.** If admission is unavailable or the sequence is missing, a pod may start without a gate. Gates cannot be added later.
- **NodeLabel:** later-group regular pods are restricted to participating nodes; first-group pods get no extra affinity. Pinned pods outside participating nodes are released.
- **Taint:** unrelated workloads may be blocked; untainted nodes and broad tolerations can bypass the restriction. DaemonSets need template tolerations, injected by the DaemonSet webhook. New nodes have a gap before tainting unless registered with the sequence taint.

## Inspect and clean up

```bash
kubectl get podseq -n default -o wide
kubectl describe podseq SEQUENCE_NAME -n default
kubectl get pods -n default -o wide
kubectl logs -n pod-sequence-system deployment/pod-sequence-controller
```

For node sequences, `status.nodeStatus` explains blocked nodes; `status.summary` gives totals. A gate timeout sets `Degraded=True` and marks the affected node `Failed`. For ungated Pending pods, inspect scheduler events with `kubectl describe pod`.

Delete demos using `kubectl delete -f` with their manifest paths, **before stopping the controller**. It needs to release gates and clean up node labels/taints. Remove the demo node label with `kubectl label node YOUR_WORKER_NODE podsequence-demo-`.

Before uninstalling, delete all sequences and remove hard-coded gates from surviving workload templates. Then remove the webhook resources (Full mode), the installed RBAC manifest, and the CRD. The RBAC manifest also deletes the controller namespace.

## Development

Go 1.21+ is required. Use `make build`, `make test`, or `make run` (local Cluster mode). After API changes, run `make generate manifests`. Build a custom image with `make docker-build IMG=YOUR_REGISTRY/pod-sequence-controller:TAG`.

Node-scope planning (per-node group satisfaction, which gated pods may start, DaemonSet "expected on node" emulation) lives in the reusable library [github.com/alvinli222/nodeseq](https://github.com/alvinli222/nodeseq). This repository adapts `PodSequence` to it and adds the CRD, webhooks, and node label/taint handling.

Contributions are welcome through pull requests. Licensed under Apache License 2.0.

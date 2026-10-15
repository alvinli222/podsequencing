package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/alvinli222/nodeseq"
	schedulingv1alpha1 "github.com/alvinli222/podsequencing/api/v1alpha1"
	"github.com/alvinli222/podsequencing/internal/sequence"
)

// TestEnvtest runs against a real kube-apiserver (no scheduler or kubelet). It verifies the CRD's CEL
// rules and that the API server accepts the controller's mutations of gated pods.
// Requires KUBEBUILDER_ASSETS (see `setup-envtest`); skipped otherwise.
func TestEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	defer func() { _ = env.Stop() }()

	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = schedulingv1alpha1.AddToScheme(s)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Run("CEL validation", func(t *testing.T) {
		bad := nodeSeq(schedulingv1alpha1.PodGroup{Pods: []string{"a"}})
		bad.Name, bad.Finalizers = "bad-explicit", nil
		if err := c.Create(ctx, bad); err == nil || !strings.Contains(err.Error(), "require podSelector") {
			t.Fatalf("explicit pods must be rejected for node scope, got %v", err)
		}
		both := clusterSeq(schedulingv1alpha1.PodGroup{Pods: []string{"a"}, PodSelector: &metav1.LabelSelector{}})
		both.Name, both.Finalizers, both.Status = "bad-both", nil, schedulingv1alpha1.PodSequenceStatus{}
		if err := c.Create(ctx, both); err == nil || !strings.Contains(err.Error(), "exactly one of") {
			t.Fatalf("pods+podSelector must be rejected, got %v", err)
		}
		ok := nodeSeq(group("csi", csiLabels))
		ok.Name, ok.Finalizers = "immutable", nil
		if err := c.Create(ctx, ok); err != nil {
			t.Fatal(err)
		}
		if ok.Spec.PodGroups[0].Policy != schedulingv1alpha1.PodGroupPolicySkipIfNotScheduled {
			t.Fatalf("policy default not applied: %q", ok.Spec.PodGroups[0].Policy)
		}
		ok.Spec.Scope = schedulingv1alpha1.PodSequenceScopeCluster
		if err := c.Update(ctx, ok); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("scope change must be rejected, got %v", err)
		}
	})

	t.Run("node-scoped reconcile against real API server", func(t *testing.T) {
		csi, gpu := daemonSet("csi", csiLabels), daemonSet("gpu", gpuLabels)
		for _, ds := range []*appsv1.DaemonSet{&csi, &gpu} {
			ds.UID = ""
			ds.Spec.Template.Spec.Containers = []corev1.Container{{Name: "c", Image: "busybox"}}
			if err := c.Create(ctx, ds); err != nil {
				t.Fatal(err)
			}
		}
		n1, n2 := node("n1", nil), node("n2", nil)
		for _, n := range []*corev1.Node{&n1, &n2} {
			if err := c.Create(ctx, n); err != nil {
				t.Fatal(err)
			}
		}
		mkPod := func(p corev1.Pod, ready bool) {
			p.Spec.Containers = []corev1.Container{{Name: "c", Image: "busybox"}}
			p.CreationTimestamp = metav1.Time{}
			status := p.Status
			if err := c.Create(ctx, &p); err != nil {
				t.Fatalf("create %s: %v", p.Name, err)
			}
			if ready {
				p.Status = status
				if err := c.Status().Update(ctx, &p); err != nil {
					t.Fatalf("status %s: %v", p.Name, err)
				}
			}
		}
		mkPod(dsPod("csi-1", csi, "n1", readyAt(time.Now().Add(-time.Minute))), true)
		mkPod(dsPod("gpu-1", gpu, "n1", gated()), false)
		mkPod(dsPod("gpu-2", gpu, "n2", gated()), false)
		web := plainPod("web-1", appLabels, gated())
		// An existing required term exercises the "only add to existing terms" rule for gated pods.
		web.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/os", Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}}}}},
		}}}
		mkPod(web, false)

		seq := nodeSeq(group("csi", csiLabels), group("gpu", gpuLabels), group("web", appLabels))
		seq.Finalizers = nil
		if err := c.Create(ctx, seq); err != nil {
			t.Fatal(err)
		}

		r := &PodSequenceReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(100), NodeScopeEnabled: true}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNS, Name: "seq"}}
		for i := 0; i < 3; i++ { // finalizer, then reconcile twice
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("reconcile %d: %v", i, err)
			}
		}

		key := sequence.EligibilityKey(testNS, "seq")
		get := func(name string) *corev1.Pod {
			p := &corev1.Pod{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: name}, p); err != nil {
				t.Fatal(err)
			}
			return p
		}
		if nodeseq.HasGate(get("gpu-1"), sequence.DefaultSchedulingGate) {
			t.Fatal("gpu-1 should be ungated (csi Ready on n1)")
		}
		if !nodeseq.HasGate(get("gpu-2"), sequence.DefaultSchedulingGate) {
			t.Fatal("gpu-2 should stay gated (no csi pod on n2)")
		}
		w := get("web-1")
		if nodeseq.HasGate(w, sequence.DefaultSchedulingGate) {
			t.Fatal("web-1 should be released")
		}
		exprs := w.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions
		if len(exprs) != 2 || exprs[1].Key != key {
			t.Fatalf("affinity not added by API server-accepted patch: %+v", exprs)
		}
		nn := &corev1.Node{}
		if err := c.Get(ctx, types.NamespacedName{Name: "n1"}, nn); err != nil || nn.Labels[key] != "1" {
			t.Fatalf("n1 label = %q (%v), want 1", nn.Labels[key], err)
		}

		// Deletion fails open.
		if err := c.Delete(ctx, seq); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		if nodeseq.HasGate(get("gpu-2"), sequence.DefaultSchedulingGate) {
			t.Fatal("gpu-2 should be released when the sequence is deleted")
		}
	})
}

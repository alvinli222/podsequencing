package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	metricNodesTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "podsequence_nodes_total",
		Help: "Number of nodes participating in a node-scoped PodSequence.",
	}, []string{"namespace", "name"})
	metricNodesSatisfied = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "podsequence_nodes_satisfied",
		Help: "Number of participating nodes on which every group is satisfied.",
	}, []string{"namespace", "name"})
	metricNodesFailed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "podsequence_nodes_failed",
		Help: "Number of nodes with a pod gated longer than gateTimeout.",
	}, []string{"namespace", "name"})
	metricGatedPods = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "podsequence_gated_pods",
		Help: "Number of member pods still holding the scheduling gate.",
	}, []string{"namespace", "name"})
	metricUngatedPods = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "podsequence_ungated_pods_total",
		Help: "Number of scheduling gates removed, by reason.",
	}, []string{"namespace", "name", "reason"})
)

func init() {
	metrics.Registry.MustRegister(metricNodesTotal, metricNodesSatisfied, metricNodesFailed, metricGatedPods, metricUngatedPods)
}

func deleteMetrics(namespace, name string) {
	l := prometheus.Labels{"namespace": namespace, "name": name}
	metricNodesTotal.Delete(l)
	metricNodesSatisfied.Delete(l)
	metricNodesFailed.Delete(l)
	metricGatedPods.Delete(l)
	metricUngatedPods.DeletePartialMatch(l)
}

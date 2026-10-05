/*
Copyright 2026 Example.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ownership

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nodesv1alpha1 "github.com/example/nodepool-controller/api/v1alpha1"
)

func TestSyncLabels_DoesNotStompForeignKeys(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "n1",
			Labels: map[string]string{
				"human":                  "true",
				"kubernetes.io/hostname": "n1",
			},
		},
	}
	if !SyncLabels(node, map[string]string{"workload": "gpu", "tier": "premium"}) {
		t.Fatal("expected change")
	}
	if node.Labels["human"] != "true" || node.Labels["kubernetes.io/hostname"] != "n1" {
		t.Fatalf("stomped foreign labels: %#v", node.Labels)
	}
	if node.Labels["workload"] != "gpu" || node.Labels["tier"] != "premium" {
		t.Fatalf("missing managed labels: %#v", node.Labels)
	}

	if !SyncLabels(node, map[string]string{"tier": "premium"}) {
		t.Fatal("expected removal of workload")
	}
	if _, ok := node.Labels["workload"]; ok {
		t.Fatal("managed label workload should have been removed")
	}
	if node.Labels["human"] != "true" {
		t.Fatal("human label should remain")
	}
}

func TestSyncLabels_OverwritesHumanValueOnOwnedKey(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "n1",
			Labels: map[string]string{"workload": "cpu"},
		},
	}
	SyncLabels(node, map[string]string{"workload": "gpu"})
	if node.Labels["workload"] != "gpu" {
		t.Fatalf("pool spec is source of truth for owned keys, got %s", node.Labels["workload"])
	}
}

func TestSyncTaints_LeavesSystemTaints(t *testing.T) {
	node := &corev1.Node{
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{{
				Key:    corev1.TaintNodeUnreachable,
				Effect: corev1.TaintEffectNoExecute,
			}},
		},
	}
	SyncTaints(node, []nodesv1alpha1.Taint{{
		Key:    "nvidia.com/gpu",
		Value:  "true",
		Effect: corev1.TaintEffectNoSchedule,
	}})
	if len(node.Spec.Taints) != 2 {
		t.Fatalf("expected 2 taints, got %#v", node.Spec.Taints)
	}
	SyncTaints(node, nil)
	if len(node.Spec.Taints) != 1 || node.Spec.Taints[0].Key != corev1.TaintNodeUnreachable {
		t.Fatalf("system taint should remain: %#v", node.Spec.Taints)
	}
}

func TestSetClaim_Conflict(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	if _, err := SetClaim(node, "pool-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := SetClaim(node, "pool-b"); err == nil {
		t.Fatal("expected conflict")
	}
}

func TestStripManaged_PreservesBootstrap(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "n1",
			Labels: map[string]string{
				nodesv1alpha1.BootstrapLabelKey: "gpu-pool",
				"workload":                      "gpu",
			},
		},
	}
	SyncLabels(node, map[string]string{"workload": "gpu"})
	StripManaged(node)
	if node.Labels[nodesv1alpha1.BootstrapLabelKey] != "gpu-pool" {
		t.Fatal("bootstrap label must survive strip")
	}
	if _, ok := node.Labels["workload"]; ok {
		t.Fatal("managed label should be gone")
	}
}

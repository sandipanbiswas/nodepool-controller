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

package controller

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nodesv1alpha1 "github.com/example/nodepool-controller/api/v1alpha1"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time { return f.t }

func newReconciler(clk Clock) *NodePoolReconciler {
	return &NodePoolReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		Clock:  clk,
	}
}

func reconcileTwice(ctx context.Context, r *NodePoolReconciler, name string) {
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
	_, err := r.Reconcile(ctx, req)
	Expect(err).NotTo(HaveOccurred())
	_, err = r.Reconcile(ctx, req)
	Expect(err).NotTo(HaveOccurred())
}

func poolSpec(action nodesv1alpha1.UnreadyAction, grace time.Duration) nodesv1alpha1.NodePoolSpec {
	return nodesv1alpha1.NodePoolSpec{
		Image: "ubuntu-22.04-gpu",
		Labels: map[string]string{
			"workload": "gpu",
			"tier":     "premium",
		},
		Taints: []nodesv1alpha1.Taint{{
			Key:    "nvidia.com/gpu",
			Value:  "true",
			Effect: corev1.TaintEffectNoSchedule,
		}},
		UnreadyPolicy: nodesv1alpha1.UnreadyPolicy{
			GracePeriod:           metav1.Duration{Duration: grace},
			Action:                action,
			MaxConcurrentRemovals: 1,
		},
	}
}

func createNode(ctx context.Context, n *corev1.Node) {
	status := n.Status.DeepCopy()
	n.Status = corev1.NodeStatus{}
	Expect(k8sClient.Create(ctx, n)).To(Succeed())
	n.Status = *status
	Expect(k8sClient.Status().Update(ctx, n)).To(Succeed())
}

func readyNode(name, pool string, ready corev1.ConditionStatus, since time.Time) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				nodesv1alpha1.BootstrapLabelKey: pool,
				"human":                         "keep-me",
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type:               corev1.NodeReady,
				Status:             ready,
				LastHeartbeatTime:  metav1.NewTime(since),
				LastTransitionTime: metav1.NewTime(since),
			}},
		},
	}
}

var _ = Describe("NodePool Controller", func() {
	ctx := context.Background()

	var (
		clk      *fakeClock
		r        *NodePoolReconciler
		poolName string
		nodeName string
	)

	BeforeEach(func() {
		clk = &fakeClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
		r = newReconciler(clk)
		poolName = "gpu-pool-" + randomSuffix()
		nodeName = "node-" + randomSuffix()
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}})
		pool := &nodesv1alpha1.NodePool{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: poolName}, pool); err == nil {
			pool.Finalizers = nil
			_ = k8sClient.Update(ctx, pool)
			_ = k8sClient.Delete(ctx, pool)
		}
	})

	It("applies labels and taints on join and counts readyNodes", func() {
		Expect(k8sClient.Create(ctx, &nodesv1alpha1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec:       poolSpec(nodesv1alpha1.UnreadyActionIgnore, time.Minute),
		})).To(Succeed())
		createNode(ctx, readyNode(nodeName, poolName, corev1.ConditionTrue, clk.t))

		reconcileTwice(ctx, r, poolName)

		node := &corev1.Node{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, node)).To(Succeed())
		Expect(node.Labels["workload"]).To(Equal("gpu"))
		Expect(node.Labels["human"]).To(Equal("keep-me"))
		Expect(node.Annotations[nodesv1alpha1.ClaimedByAnnotation]).To(Equal(poolName))
		Expect(node.Spec.Taints).To(ContainElement(corev1.Taint{
			Key: "nvidia.com/gpu", Value: "true", Effect: corev1.TaintEffectNoSchedule,
		}))

		pool := &nodesv1alpha1.NodePool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: poolName}, pool)).To(Succeed())
		Expect(pool.Status.ReadyNodes).To(Equal(int32(1)))
		Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
		Expect(pool.Status.Nodes[0].Phase).To(Equal(nodesv1alpha1.NodePhaseReady))
	})

	It("propagates spec changes without removing foreign labels", func() {
		Expect(k8sClient.Create(ctx, &nodesv1alpha1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec:       poolSpec(nodesv1alpha1.UnreadyActionIgnore, time.Minute),
		})).To(Succeed())
		createNode(ctx, readyNode(nodeName, poolName, corev1.ConditionTrue, clk.t))
		reconcileTwice(ctx, r, poolName)

		pool := &nodesv1alpha1.NodePool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: poolName}, pool)).To(Succeed())
		pool.Spec.Labels = map[string]string{"tier": "premium"}
		Expect(k8sClient.Update(ctx, pool)).To(Succeed())
		reconcileTwice(ctx, r, poolName)

		node := &corev1.Node{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, node)).To(Succeed())
		Expect(node.Labels).NotTo(HaveKey("workload"))
		Expect(node.Labels["tier"]).To(Equal("premium"))
		Expect(node.Labels["human"]).To(Equal("keep-me"))
	})

	It("removes a NotReady node after gracePeriod when action=Remove", func() {
		Expect(k8sClient.Create(ctx, &nodesv1alpha1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec:       poolSpec(nodesv1alpha1.UnreadyActionRemove, 5*time.Minute),
		})).To(Succeed())
		createNode(ctx, readyNode(nodeName, poolName, corev1.ConditionUnknown, clk.t))
		reconcileTwice(ctx, r, poolName)

		pool := &nodesv1alpha1.NodePool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: poolName}, pool)).To(Succeed())
		Expect(pool.Status.Nodes[0].Phase).To(Equal(nodesv1alpha1.NodePhaseNotReady))
		Expect(pool.Status.Nodes[0].NotReadySince).NotTo(BeNil())

		clk.t = clk.t.Add(6 * time.Minute)
		reconcileTwice(ctx, r, poolName)

		err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, &corev1.Node{})
		Expect(errors.IsNotFound(err)).To(BeTrue())
	})

	It("respects maxConcurrentRemovals", func() {
		n2 := "node-b-" + randomSuffix()
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n2}})
		})
		spec := poolSpec(nodesv1alpha1.UnreadyActionRemove, time.Minute)
		spec.UnreadyPolicy.MaxConcurrentRemovals = 1
		Expect(k8sClient.Create(ctx, &nodesv1alpha1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec:       spec,
		})).To(Succeed())
		createNode(ctx, readyNode(nodeName, poolName, corev1.ConditionFalse, clk.t))
		createNode(ctx, readyNode(n2, poolName, corev1.ConditionFalse, clk.t))
		reconcileTwice(ctx, r, poolName)
		clk.t = clk.t.Add(2 * time.Minute)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: poolName}})
		Expect(err).NotTo(HaveOccurred())

		var remaining int
		for _, n := range []string{nodeName, n2} {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: n}, &corev1.Node{}); err == nil {
				remaining++
			}
		}
		Expect(remaining).To(Equal(1))
	})

	It("strips managed labels on pool deletion and keeps bootstrap", func() {
		Expect(k8sClient.Create(ctx, &nodesv1alpha1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec:       poolSpec(nodesv1alpha1.UnreadyActionIgnore, time.Minute),
		})).To(Succeed())
		createNode(ctx, readyNode(nodeName, poolName, corev1.ConditionTrue, clk.t))
		reconcileTwice(ctx, r, poolName)

		pool := &nodesv1alpha1.NodePool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: poolName}, pool)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pool)).To(Succeed())
		reconcileTwice(ctx, r, poolName)

		node := &corev1.Node{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, node)).To(Succeed())
		Expect(node.Labels).NotTo(HaveKey("workload"))
		Expect(node.Labels[nodesv1alpha1.BootstrapLabelKey]).To(Equal(poolName))
		Expect(node.Labels["human"]).To(Equal("keep-me"))
		err := k8sClient.Get(ctx, types.NamespacedName{Name: poolName}, &nodesv1alpha1.NodePool{})
		Expect(errors.IsNotFound(err)).To(BeTrue())
	})

	It("does not steal a node claimed by another pool", func() {
		other := "other-" + randomSuffix()
		Expect(k8sClient.Create(ctx, &nodesv1alpha1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec:       poolSpec(nodesv1alpha1.UnreadyActionIgnore, time.Minute),
		})).To(Succeed())
		n := readyNode(nodeName, poolName, corev1.ConditionTrue, clk.t)
		n.Annotations = map[string]string{nodesv1alpha1.ClaimedByAnnotation: other}
		createNode(ctx, n)
		reconcileTwice(ctx, r, poolName)

		node := &corev1.Node{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, node)).To(Succeed())
		Expect(node.Labels).NotTo(HaveKey("workload"))
		pool := &nodesv1alpha1.NodePool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: poolName}, pool)).To(Succeed())
		Expect(pool.Status.Conditions).NotTo(BeEmpty())
	})
})

var testSeq atomic.Uint64

func randomSuffix() string {
	return fmt.Sprintf("%d", testSeq.Add(1))
}

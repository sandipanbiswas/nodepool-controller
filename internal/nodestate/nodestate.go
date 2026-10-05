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

package nodestate

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nodesv1alpha1 "github.com/example/nodepool-controller/api/v1alpha1"
)

// ReadyCondition returns the Node Ready condition, or a zero value if missing.
func ReadyCondition(node *corev1.Node) corev1.NodeCondition {
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == corev1.NodeReady {
			return node.Status.Conditions[i]
		}
	}
	return corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}
}

// IsNodeReady is true only when Ready=True.
func IsNodeReady(node *corev1.Node) bool {
	return ReadyCondition(node).Status == corev1.ConditionTrue
}

// PreviousMember finds persisted member status by name.
func PreviousMember(pool *nodesv1alpha1.NodePool, name string) *nodesv1alpha1.NodeMemberStatus {
	for i := range pool.Status.Nodes {
		if pool.Status.Nodes[i].Name == name {
			return &pool.Status.Nodes[i]
		}
	}
	return nil
}

// NotReadySince chooses a clock that survives restart: existing status, else Node condition timestamp, else now.
func NotReadySince(prev *nodesv1alpha1.NodeMemberStatus, cond corev1.NodeCondition, now time.Time) *metav1.Time {
	if prev != nil && prev.NotReadySince != nil && !prev.NotReadySince.IsZero() {
		return prev.NotReadySince.DeepCopy()
	}
	if !cond.LastTransitionTime.IsZero() {
		t := cond.LastTransitionTime
		return &t
	}
	t := metav1.NewTime(now)
	return &t
}

// RemainingGrace returns how long until grace expires. Zero or negative means expired.
func RemainingGrace(since *metav1.Time, grace time.Duration, now time.Time) time.Duration {
	if since == nil {
		return grace
	}
	deadline := since.Time.Add(grace)
	return deadline.Sub(now)
}

func InFlightRemoval(phase nodesv1alpha1.NodePhase) bool {
	return phase == nodesv1alpha1.NodePhaseDraining || phase == nodesv1alpha1.NodePhaseRemoving
}

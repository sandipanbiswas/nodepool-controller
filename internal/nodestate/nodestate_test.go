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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nodesv1alpha1 "github.com/example/nodepool-controller/api/v1alpha1"
)

func TestRemainingGrace(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	since := metav1.NewTime(now.Add(-2 * time.Minute))
	left := RemainingGrace(&since, 5*time.Minute, now)
	if left != 3*time.Minute {
		t.Fatalf("got %s", left)
	}
	if RemainingGrace(&since, 1*time.Minute, now) > 0 {
		t.Fatal("expected expired")
	}
}

func TestNotReadySince_PrefersStatusOverCondition(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	statusTime := metav1.NewTime(now.Add(-10 * time.Minute))
	condTime := metav1.NewTime(now.Add(-1 * time.Minute))
	prev := &nodesv1alpha1.NodeMemberStatus{NotReadySince: &statusTime}
	cond := corev1.NodeCondition{LastTransitionTime: condTime}
	got := NotReadySince(prev, cond, now)
	if !got.Equal(&statusTime) {
		t.Fatalf("status must win across restart, got %s", got.Time)
	}
}

func TestIsNodeReady(t *testing.T) {
	n := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
		Type:   corev1.NodeReady,
		Status: corev1.ConditionUnknown,
	}}}}
	if IsNodeReady(n) {
		t.Fatal("Unknown is not Ready")
	}
}

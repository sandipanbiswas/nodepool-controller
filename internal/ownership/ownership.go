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
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	nodesv1alpha1 "github.com/example/nodepool-controller/api/v1alpha1"
)

// Reserved label prefixes that kubelet / CCM / Kubernetes own. We never write or delete these.
var reservedLabelPrefixes = []string{
	"kubernetes.io/",
	"k8s.io/",
	"node.kubernetes.io/",
	"beta.kubernetes.io/",
	"node-restriction.kubernetes.io/",
}

var reservedTaintPrefixes = []string{
	"node.kubernetes.io/",
	"node.cloudprovider.kubernetes.io/",
}

// IsReservedLabel reports whether the controller must not manage this key.
func IsReservedLabel(key string) bool {
	if key == nodesv1alpha1.BootstrapLabelKey {
		return true
	}
	for _, p := range reservedLabelPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// IsReservedTaint reports whether the controller must not manage this taint.
func IsReservedTaint(key string) bool {
	for _, p := range reservedTaintPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func taintID(key string, effect corev1.TaintEffect) string {
	return key + ":" + string(effect)
}

func parseJSONStringSlice(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func encodeJSONStringSlice(values []string) string {
	sort.Strings(values)
	b, err := json.Marshal(values)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func ensureAnnotations(node *corev1.Node) {
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
}

// SyncLabels applies desired pool labels and removes labels this controller previously owned
// that are no longer desired. Labels applied by other actors are left alone.
func SyncLabels(node *corev1.Node, desired map[string]string) bool {
	ensureAnnotations(node)
	managed := parseJSONStringSlice(node.Annotations[nodesv1alpha1.ManagedLabelsAnnotation])

	changed := false
	nextManaged := make([]string, 0, len(desired))

	for _, k := range managed {
		if IsReservedLabel(k) {
			continue
		}
		if _, still := desired[k]; still {
			continue
		}
		if _, exists := node.Labels[k]; exists {
			delete(node.Labels, k)
			changed = true
		}
	}

	for k, v := range desired {
		if IsReservedLabel(k) {
			continue
		}
		if cur, ok := node.Labels[k]; !ok || cur != v {
			node.Labels[k] = v
			changed = true
		}
		nextManaged = append(nextManaged, k)
	}

	encoded := encodeJSONStringSlice(nextManaged)
	if node.Annotations[nodesv1alpha1.ManagedLabelsAnnotation] != encoded {
		node.Annotations[nodesv1alpha1.ManagedLabelsAnnotation] = encoded
		changed = true
	}
	return changed
}

// SyncTaints applies desired pool taints and removes taints previously owned that are gone.
func SyncTaints(node *corev1.Node, desired []nodesv1alpha1.Taint) bool {
	ensureAnnotations(node)
	managed := parseJSONStringSlice(node.Annotations[nodesv1alpha1.ManagedTaintsAnnotation])
	managedSet := map[string]struct{}{}
	for _, id := range managed {
		managedSet[id] = struct{}{}
	}

	desiredIDs := map[string]nodesv1alpha1.Taint{}
	nextManaged := make([]string, 0, len(desired))
	for _, t := range desired {
		if IsReservedTaint(t.Key) {
			continue
		}
		id := taintID(t.Key, t.Effect)
		desiredIDs[id] = t
		nextManaged = append(nextManaged, id)
	}

	changed := false
	kept := make([]corev1.Taint, 0, len(node.Spec.Taints))
	for _, existing := range node.Spec.Taints {
		id := taintID(existing.Key, existing.Effect)
		if want, ok := desiredIDs[id]; ok {
			if existing.Value != want.Value {
				existing.Value = want.Value
				changed = true
			}
			kept = append(kept, existing)
			delete(desiredIDs, id)
			continue
		}
		if _, owned := managedSet[id]; owned {
			changed = true
			continue
		}
		kept = append(kept, existing)
	}

	for _, t := range desiredIDs {
		kept = append(kept, t.ToCore())
		changed = true
	}

	node.Spec.Taints = kept
	encoded := encodeJSONStringSlice(nextManaged)
	if node.Annotations[nodesv1alpha1.ManagedTaintsAnnotation] != encoded {
		node.Annotations[nodesv1alpha1.ManagedTaintsAnnotation] = encoded
		changed = true
	}
	return changed
}

// StripManaged removes labels and taints recorded as owned by this controller.
func StripManaged(node *corev1.Node) bool {
	ensureAnnotations(node)
	changed := false
	for _, k := range parseJSONStringSlice(node.Annotations[nodesv1alpha1.ManagedLabelsAnnotation]) {
		if IsReservedLabel(k) {
			continue
		}
		if _, ok := node.Labels[k]; ok {
			delete(node.Labels, k)
			changed = true
		}
	}
	managedTaints := map[string]struct{}{}
	for _, id := range parseJSONStringSlice(node.Annotations[nodesv1alpha1.ManagedTaintsAnnotation]) {
		managedTaints[id] = struct{}{}
	}
	if len(managedTaints) > 0 {
		kept := make([]corev1.Taint, 0, len(node.Spec.Taints))
		for _, t := range node.Spec.Taints {
			if _, owned := managedTaints[taintID(t.Key, t.Effect)]; owned {
				changed = true
				continue
			}
			kept = append(kept, t)
		}
		node.Spec.Taints = kept
	}
	for _, key := range []string{
		nodesv1alpha1.ManagedLabelsAnnotation,
		nodesv1alpha1.ManagedTaintsAnnotation,
		nodesv1alpha1.ClaimedByAnnotation,
		nodesv1alpha1.CordonedAnnotation,
	} {
		if _, ok := node.Annotations[key]; ok {
			delete(node.Annotations, key)
			changed = true
		}
	}
	return changed
}

// SetClaim records pool ownership. Returns an error if another pool already claimed the node.
func SetClaim(node *corev1.Node, poolName string) (bool, error) {
	ensureAnnotations(node)
	current := node.Annotations[nodesv1alpha1.ClaimedByAnnotation]
	if current != "" && current != poolName {
		return false, fmt.Errorf("node %q is claimed by NodePool %q", node.Name, current)
	}
	if current == poolName {
		return false, nil
	}
	node.Annotations[nodesv1alpha1.ClaimedByAnnotation] = poolName
	return true, nil
}

// ReleaseClaim clears claimed-by if it matches poolName.
func ReleaseClaim(node *corev1.Node, poolName string) bool {
	ensureAnnotations(node)
	if node.Annotations[nodesv1alpha1.ClaimedByAnnotation] != poolName {
		return false
	}
	delete(node.Annotations, nodesv1alpha1.ClaimedByAnnotation)
	return true
}

// MarkCordoned records that this controller set unschedulable.
func MarkCordoned(node *corev1.Node) bool {
	ensureAnnotations(node)
	changed := false
	if !node.Spec.Unschedulable {
		node.Spec.Unschedulable = true
		changed = true
	}
	if node.Annotations[nodesv1alpha1.CordonedAnnotation] != "true" {
		node.Annotations[nodesv1alpha1.CordonedAnnotation] = "true"
		changed = true
	}
	return changed
}

// UncordonIfWeCordoned reverses our cordon only.
func UncordonIfWeCordoned(node *corev1.Node) bool {
	ensureAnnotations(node)
	if node.Annotations[nodesv1alpha1.CordonedAnnotation] != "true" {
		return false
	}
	if node.Spec.Unschedulable {
		node.Spec.Unschedulable = false
	}
	delete(node.Annotations, nodesv1alpha1.CordonedAnnotation)
	return true
}

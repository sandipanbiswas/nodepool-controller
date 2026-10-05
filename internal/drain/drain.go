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

package drain

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Result of one drain attempt.
type Result struct {
	Pending int
	Evicted int
}

func isDaemonSetPod(pod *corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "DaemonSet" && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

func isMirrorPod(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[corev1.MirrorPodAnnotationKey]
	return ok
}

// SkipReason is empty if the pod should be evicted.
func SkipReason(pod *corev1.Pod) string {
	if pod.DeletionTimestamp != nil {
		return "terminating"
	}
	if isMirrorPod(pod) {
		return "mirror"
	}
	if isDaemonSetPod(pod) {
		return "daemonset"
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return "finished"
	}
	return ""
}

// EvictablePods on a node that drain should wait for.
func EvictablePods(ctx context.Context, c client.Client, nodeName string) ([]corev1.Pod, error) {
	var list corev1.PodList
	if err := c.List(ctx, &list, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		// Field indexer may be missing (direct envtest client). Fall back to full list.
		if err := c.List(ctx, &list); err != nil {
			return nil, err
		}
		filtered := make([]corev1.Pod, 0, len(list.Items))
		for _, p := range list.Items {
			if p.Spec.NodeName == nodeName {
				filtered = append(filtered, p)
			}
		}
		list.Items = filtered
	}
	out := make([]corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		if SkipReason(&list.Items[i]) == "" {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

// EvictPods creates Eviction objects. PDB denials surface as 429/Conflict so the caller can requeue.
func EvictPods(ctx context.Context, c client.Client, pods []corev1.Pod) (Result, error) {
	var res Result
	var errs []error
	for i := range pods {
		pod := pods[i]
		ev := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
		}
		err := c.SubResource("eviction").Create(ctx, &pod, ev)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			if apierrors.IsTooManyRequests(err) || apierrors.IsConflict(err) {
				res.Pending++
				errs = append(errs, fmt.Errorf("evict %s/%s: %w", pod.Namespace, pod.Name, err))
				continue
			}
			// envtest may not serve the eviction subresource; fall back to a normal delete.
			if err2 := c.Delete(ctx, &pod); err2 != nil && !apierrors.IsNotFound(err2) {
				res.Pending++
				errs = append(errs, fmt.Errorf("delete %s/%s: %w", pod.Namespace, pod.Name, err2))
				continue
			}
			res.Evicted++
			continue
		}
		res.Evicted++
	}
	return res, errors.Join(errs...)
}

// ForceDeletePods immediately deletes remaining pods (grace period 0).
func ForceDeletePods(ctx context.Context, c client.Client, pods []corev1.Pod) error {
	zero := int64(0)
	var errs []error
	for i := range pods {
		p := pods[i]
		if err := c.Delete(ctx, &p, &client.DeleteOptions{GracePeriodSeconds: &zero}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

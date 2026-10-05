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
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nodesv1alpha1 "github.com/example/nodepool-controller/api/v1alpha1"
	"github.com/example/nodepool-controller/internal/drain"
	"github.com/example/nodepool-controller/internal/nodestate"
	"github.com/example/nodepool-controller/internal/ownership"
)

const (
	eventNodeJoined           = "NodeJoined"
	eventNodeNotReady         = "NodeNotReady"
	eventNodeReady            = "NodeReady"
	eventNodeCordoned         = "NodeCordoned"
	eventNodeDraining         = "NodeDraining"
	eventNodeRemoving         = "NodeRemoving"
	eventNodeRemoved          = "NodeRemoved"
	eventMembershipConflict   = "MembershipConflict"
	eventDrainTimeout         = "DrainTimeout"
	eventPoolDeleting         = "PoolDeleting"
	eventUnreadyActionIgnored = "UnreadyIgnored"
)

// Clock abstracts time for grace-period tests.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// NodePoolReconciler reconciles a NodePool object.
type NodePoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Clock    Clock
}

func (r *NodePoolReconciler) now() time.Time {
	if r.Clock == nil {
		return time.Now()
	}
	return r.Clock.Now()
}

func (r *NodePoolReconciler) event(obj runtime.Object, etype, reason, msg string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(obj, etype, reason, msg)
}

// +kubebuilder:rbac:groups=nodes.example.com,resources=nodepools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nodes.example.com,resources=nodepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nodes.example.com,resources=nodepools/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *NodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	pool := &nodesv1alpha1.NodePool{}
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	members, err := r.listMemberNodes(ctx, pool)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !pool.DeletionTimestamp.IsZero() {
		if err := r.cleanupOnDelete(ctx, pool, members); err != nil {
			return ctrl.Result{}, err
		}
		if controllerutil.ContainsFinalizer(pool, nodesv1alpha1.FinalizerName) {
			controllerutil.RemoveFinalizer(pool, nodesv1alpha1.FinalizerName)
			if err := r.Update(ctx, pool); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(pool, nodesv1alpha1.FinalizerName) {
		controllerutil.AddFinalizer(pool, nodesv1alpha1.FinalizerName)
		if err := r.Update(ctx, pool); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	inFlight := 0
	for _, prev := range pool.Status.Nodes {
		if nodestate.InFlightRemoval(prev.Phase) {
			inFlight++
		}
	}

	requeueAfter := time.Duration(0)
	failed := false
	failMsg := ""
	statusMembers := make([]nodesv1alpha1.NodeMemberStatus, 0, len(members))
	readyCount := int32(0)
	seen := map[string]struct{}{}

	for i := range members {
		node := members[i].DeepCopy()
		prev := nodestate.PreviousMember(pool, node.Name)
		prevPhase := nodesv1alpha1.NodePhase("")
		if prev != nil {
			prevPhase = prev.Phase
		}

		bootstrap := node.Labels[nodesv1alpha1.BootstrapLabelKey]
		claimed := node.Annotations[nodesv1alpha1.ClaimedByAnnotation]

		if claimed != "" && claimed != pool.Name {
			failed = true
			failMsg = fmt.Sprintf("node %s is claimed by %s", node.Name, claimed)
			r.event(pool, corev1.EventTypeWarning, eventMembershipConflict, failMsg)
			continue
		}

		if bootstrap != "" && bootstrap != pool.Name {
			if claimed == pool.Name {
				if ownership.StripManaged(node) {
					if err := r.Update(ctx, node); err != nil {
						return ctrl.Result{}, err
					}
				}
			}
			continue
		}

		changed, err := ownership.SetClaim(node, pool.Name)
		if err != nil {
			failed = true
			failMsg = err.Error()
			r.event(pool, corev1.EventTypeWarning, eventMembershipConflict, failMsg)
			continue
		}
		if ownership.SyncLabels(node, pool.Spec.Labels) {
			changed = true
		}
		if ownership.SyncTaints(node, pool.Spec.Taints) {
			changed = true
		}

		ready := nodestate.IsNodeReady(node)
		cond := nodestate.ReadyCondition(node)
		member := nodesv1alpha1.NodeMemberStatus{Name: node.Name}

		if ready {
			if prevPhase != "" && prevPhase != nodesv1alpha1.NodePhaseReady {
				r.event(pool, corev1.EventTypeNormal, eventNodeReady, fmt.Sprintf("node %s recovered", node.Name))
			}
			if prevPhase == "" {
				r.event(pool, corev1.EventTypeNormal, eventNodeJoined, fmt.Sprintf("node %s joined pool", node.Name))
			}
			if ownership.UncordonIfWeCordoned(node) {
				changed = true
			}
			member.Phase = nodesv1alpha1.NodePhaseReady
			readyCount++
			if changed {
				if err := r.Update(ctx, node); err != nil {
					return ctrl.Result{}, err
				}
			}
			statusMembers = append(statusMembers, member)
			seen[node.Name] = struct{}{}
			continue
		}

		member.NotReadySince = nodestate.NotReadySince(prev, cond, r.now())
		if prev == nil || prev.Phase == nodesv1alpha1.NodePhaseReady || prev.Phase == "" {
			r.event(pool, corev1.EventTypeWarning, eventNodeNotReady,
				fmt.Sprintf("node %s Ready=%s", node.Name, cond.Status))
		}

		graceLeft := nodestate.RemainingGrace(member.NotReadySince, pool.Spec.UnreadyPolicy.GracePeriod.Duration, r.now())
		phase := nodesv1alpha1.NodePhaseNotReady
		if prev != nil && nodestate.InFlightRemoval(prev.Phase) {
			phase = prev.Phase
			member.DrainStartedAt = prev.DrainStartedAt
		}

		if graceLeft > 0 && !nodestate.InFlightRemoval(phase) {
			member.Phase = nodesv1alpha1.NodePhaseNotReady
			if changed {
				if err := r.Update(ctx, node); err != nil {
					return ctrl.Result{}, err
				}
			}
			statusMembers = append(statusMembers, member)
			seen[node.Name] = struct{}{}
			requeueAfter = minPositive(requeueAfter, graceLeft)
			continue
		}

		switch pool.Spec.UnreadyPolicy.Action {
		case nodesv1alpha1.UnreadyActionIgnore:
			r.event(pool, corev1.EventTypeNormal, eventUnreadyActionIgnored,
				fmt.Sprintf("node %s past grace; action=Ignore", node.Name))
			member.Phase = nodesv1alpha1.NodePhaseNotReady
		case nodesv1alpha1.UnreadyActionCordon:
			if ownership.MarkCordoned(node) {
				changed = true
				r.event(pool, corev1.EventTypeNormal, eventNodeCordoned, fmt.Sprintf("cordoned node %s", node.Name))
			}
			member.Phase = nodesv1alpha1.NodePhaseNotReady
		case nodesv1alpha1.UnreadyActionRemove:
			max := pool.Spec.UnreadyPolicy.MaxConcurrentRemovals
			if max <= 0 {
				max = 1 // CRD default; safety rail from the spec
			}
			canStart := nodestate.InFlightRemoval(phase) || inFlight < int(max)
			if !canStart {
				member.Phase = nodesv1alpha1.NodePhaseNotReady
				break
			}
			if !nodestate.InFlightRemoval(phase) {
				inFlight++
				phase = nodesv1alpha1.NodePhaseDraining
				t := metav1.NewTime(r.now())
				member.DrainStartedAt = &t
				r.event(pool, corev1.EventTypeNormal, eventNodeDraining, fmt.Sprintf("draining node %s", node.Name))
			}
			if ownership.MarkCordoned(node) {
				changed = true
			}
			if changed {
				if err := r.Update(ctx, node); err != nil {
					return ctrl.Result{}, err
				}
				changed = false
			}

			pods, err := drain.EvictablePods(ctx, r.Client, node.Name)
			if err != nil {
				return ctrl.Result{}, err
			}
			drainTO := pool.Spec.UnreadyPolicy.DrainTimeoutDuration().Duration
			started := r.now()
			if member.DrainStartedAt != nil {
				started = member.DrainStartedAt.Time
			}
			timedOut := r.now().After(started.Add(drainTO))

			if len(pods) > 0 && !timedOut {
				if _, err := drain.EvictPods(ctx, r.Client, pods); err != nil {
					log.Info("eviction pending", "node", node.Name, "err", err)
				}
				member.Phase = nodesv1alpha1.NodePhaseDraining
				requeueAfter = minPositive(requeueAfter, 2*time.Second)
				break
			}

			if len(pods) > 0 && timedOut {
				r.event(pool, corev1.EventTypeWarning, eventDrainTimeout,
					fmt.Sprintf("drain timeout on %s; %d pods remain", node.Name, len(pods)))
				if pool.Spec.UnreadyPolicy.ForceDeletePodsAfterDrainTimeout && cond.Status == corev1.ConditionUnknown {
					if err := drain.ForceDeletePods(ctx, r.Client, pods); err != nil {
						log.Error(err, "force delete pods", "node", node.Name)
						failed = true
						failMsg = err.Error()
						member.Phase = nodesv1alpha1.NodePhaseDraining
						break
					}
				}
			}

			member.Phase = nodesv1alpha1.NodePhaseRemoving
			r.event(pool, corev1.EventTypeNormal, eventNodeRemoving, fmt.Sprintf("deleting Node %s", node.Name))
			if err := r.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			// Keep member in status until the object is gone so maxConcurrentRemovals still counts.
			statusMembers = append(statusMembers, member)
			seen[node.Name] = struct{}{}
			continue
		}

		if changed {
			if err := r.Update(ctx, node); err != nil {
				return ctrl.Result{}, err
			}
		}
		statusMembers = append(statusMembers, member)
		seen[node.Name] = struct{}{}
	}

	for _, prev := range pool.Status.Nodes {
		if _, ok := seen[prev.Name]; ok {
			continue
		}
		var n corev1.Node
		err := r.Get(ctx, client.ObjectKey{Name: prev.Name}, &n)
		if apierrors.IsNotFound(err) {
			r.event(pool, corev1.EventTypeNormal, eventNodeRemoved, fmt.Sprintf("node %s gone", prev.Name))
			continue
		}
	}

	sort.Slice(statusMembers, func(i, j int) bool { return statusMembers[i].Name < statusMembers[j].Name })

	progressing := false
	for _, m := range statusMembers {
		if nodestate.InFlightRemoval(m.Phase) {
			progressing = true
			break
		}
	}

	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               nodesv1alpha1.ConditionReady,
		Status:             boolCond(failed == false && !progressing),
		Reason:             readyReason(failed, progressing, readyCount),
		Message:            readyMessage(failed, failMsg, readyCount, len(statusMembers)),
		ObservedGeneration: pool.Generation,
	})
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               nodesv1alpha1.ConditionProgressing,
		Status:             boolCond(progressing),
		Reason:             progressingReason(progressing),
		Message:            progressingMessage(progressing),
		ObservedGeneration: pool.Generation,
	})
	failStatus := metav1.ConditionFalse
	if failed {
		failStatus = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               nodesv1alpha1.ConditionFailed,
		Status:             failStatus,
		Reason:             failedReason(failed),
		Message:            failMsg,
		ObservedGeneration: pool.Generation,
	})

	pool.Status.ReadyNodes = readyCount
	pool.Status.Nodes = statusMembers
	pool.Status.ObservedGeneration = pool.Generation
	if err := r.Status().Update(ctx, pool); err != nil {
		return ctrl.Result{}, err
	}

	if requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

func (r *NodePoolReconciler) cleanupOnDelete(ctx context.Context, pool *nodesv1alpha1.NodePool, members []corev1.Node) error {
	r.event(pool, corev1.EventTypeNormal, eventPoolDeleting, "stripping managed labels/taints from members")
	for i := range members {
		node := members[i].DeepCopy()
		if node.Labels[nodesv1alpha1.BootstrapLabelKey] != pool.Name &&
			node.Annotations[nodesv1alpha1.ClaimedByAnnotation] != pool.Name {
			continue
		}
		uncordoned := false
		if nodestate.IsNodeReady(node) {
			uncordoned = ownership.UncordonIfWeCordoned(node)
		}
		stripped := ownership.StripManaged(node)
		if uncordoned || stripped {
			if err := r.Update(ctx, node); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *NodePoolReconciler) listMemberNodes(ctx context.Context, pool *nodesv1alpha1.NodePool) ([]corev1.Node, error) {
	var labeled corev1.NodeList
	if err := r.List(ctx, &labeled, client.MatchingLabels{nodesv1alpha1.BootstrapLabelKey: pool.Name}); err != nil {
		return nil, err
	}
	byName := map[string]corev1.Node{}
	for _, n := range labeled.Items {
		byName[n.Name] = n
	}
	for _, m := range pool.Status.Nodes {
		if _, ok := byName[m.Name]; ok {
			continue
		}
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: m.Name}, &node); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if node.Labels[nodesv1alpha1.BootstrapLabelKey] == pool.Name ||
			node.Annotations[nodesv1alpha1.ClaimedByAnnotation] == pool.Name {
			byName[node.Name] = node
		}
	}
	out := make([]corev1.Node, 0, len(byName))
	for _, n := range byName {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *NodePoolReconciler) mapNodeToPool(_ context.Context, obj client.Object) []reconcile.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}
	names := map[string]struct{}{}
	if v := node.Labels[nodesv1alpha1.BootstrapLabelKey]; v != "" {
		names[v] = struct{}{}
	}
	if v := node.Annotations[nodesv1alpha1.ClaimedByAnnotation]; v != "" {
		names[v] = struct{}{}
	}
	reqs := make([]reconcile.Request, 0, len(names))
	for name := range names {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKey{Name: name}})
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("nodepool-controller")
	}
	if r.Clock == nil {
		r.Clock = realClock{}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&nodesv1alpha1.NodePool{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapNodeToPool)).
		Named("nodepool").
		Complete(r)
}

func minPositive(cur, next time.Duration) time.Duration {
	if next <= 0 {
		return cur
	}
	if cur <= 0 || next < cur {
		return next
	}
	return cur
}

func boolCond(v bool) metav1.ConditionStatus {
	if v {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func readyReason(failed, progressing bool, ready int32) string {
	if failed {
		return "Error"
	}
	if progressing {
		return "InProgress"
	}
	if ready == 0 {
		return "NoReadyNodes"
	}
	return "MembersReady"
}

func readyMessage(failed bool, failMsg string, ready int32, total int) string {
	if failed {
		return failMsg
	}
	return fmt.Sprintf("%d/%d members Ready", ready, total)
}

func progressingReason(p bool) string {
	if p {
		return "Draining"
	}
	return "Idle"
}

func progressingMessage(p bool) string {
	if p {
		return "one or more members are draining or being removed"
	}
	return "no in-flight removals"
}

func failedReason(f bool) string {
	if f {
		return "Conflict"
	}
	return "NoError"
}

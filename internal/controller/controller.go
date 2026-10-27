// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package controller reconciles ExtensionManaged MonitoringBindings of a
// victoriametrics MonitoringClass into VictoriaMetrics Operator VMPodScrapes.
package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

// FieldOwner is the server-side-apply field manager of everything this
// controller writes.
const FieldOwner = "openeverest.io/monitoring-victoriametrics"

// applyRetryInterval paces retries while VMPodScrapes cannot be written, for
// example until the VictoriaMetrics Operator CRDs are installed.
const applyRetryInterval = 30 * time.Second

// Setup registers the controllers serving the MonitoringClass className: one
// claims the class, one reports destination readiness and one renders the
// VMPodScrapes of every binding. VMPodScrapes are only ever written, never
// watched, so the manager starts without the VictoriaMetrics Operator CRDs.
func Setup(mgr ctrl.Manager, className string) error {
	cc := monitoring.ClassController{Client: mgr.GetClient(), FieldOwner: FieldOwner, ClassName: className}

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-victoriametrics-class").
		For(&monitoringv1alpha1.MonitoringClass{}, builder.WithPredicates(
			predicate.GenerationChangedPredicate{},
			predicate.NewPredicateFuncs(func(obj client.Object) bool {
				class, ok := obj.(*monitoringv1alpha1.MonitoringClass)
				return ok && cc.OwnsClass(class)
			}),
		)).
		Complete(&classReconciler{client: mgr.GetClient(), cc: cc}); err != nil {
		return fmt.Errorf("set up MonitoringClass controller: %w", err)
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-victoriametrics-destination").
		For(&monitoringv1alpha1.MonitoringDestination{}, builder.WithPredicates(
			predicate.GenerationChangedPredicate{},
			predicate.NewPredicateFuncs(func(obj client.Object) bool {
				dst, ok := obj.(*monitoringv1alpha1.MonitoringDestination)
				return ok && dst.Spec.ClassRef.Name == className
			}),
		)).
		Complete(&destinationReconciler{client: mgr.GetClient(), cc: cc}); err != nil {
		return fmt.Errorf("set up MonitoringDestination controller: %w", err)
	}

	r := &bindingReconciler{client: mgr.GetClient(), cc: cc, className: className}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-victoriametrics").
		For(&monitoringv1alpha1.MonitoringBinding{}, builder.WithPredicates(monitoring.BindingPredicate(className))).
		Watches(&corev1alpha1.Instance{}, handler.EnqueueRequestsFromMapFunc(r.bindingsForInstance),
			builder.WithPredicates(sourcesChanged())).
		Watches(&monitoringv1alpha1.MonitoringDestination{}, handler.EnqueueRequestsFromMapFunc(r.bindingsForDestination),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r); err != nil {
		return fmt.Errorf("set up MonitoringBinding controller: %w", err)
	}
	return nil
}

// classReconciler claims the served class and leaves any other unclaimed.
type classReconciler struct {
	client client.Client
	cc     monitoring.ClassController
}

func (r *classReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	class := &monitoringv1alpha1.MonitoringClass{}
	if err := r.client.Get(ctx, req.NamespacedName, class); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, r.cc.ClaimClass(ctx, class)
}

// destinationReconciler reports Ready on destinations of our class. There is
// no server to probe: a destination is ready once its parameters can be
// rendered into VMPodScrapes.
type destinationReconciler struct {
	client client.Client
	cc     monitoring.ClassController
}

func (r *destinationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	dst := &monitoringv1alpha1.MonitoringDestination{}
	if err := r.client.Get(ctx, req.NamespacedName, dst); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if dst.Spec.ClassRef.Name != r.cc.ClassName {
		return ctrl.Result{}, nil
	}
	params, err := decodeParameters(dst.Spec.Parameters)
	if err != nil {
		return ctrl.Result{}, r.cc.SetDestinationReady(ctx, dst, monitoringv1alpha1.ReasonReadyInvalidParameters, err.Error(), "")
	}
	return ctrl.Result{}, r.cc.SetDestinationReady(ctx, dst, monitoringv1alpha1.ReasonReady,
		"VMPodScrapes are labelled "+labelsString(params.VMPodScrapeLabels), "")
}

// sourcesChanged passes Instance updates that change
// status.monitoring.sources, the only Instance field VMPodScrapes derive
// from.
func sourcesChanged() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldIn, okOld := e.ObjectOld.(*corev1alpha1.Instance)
			newIn, okNew := e.ObjectNew.(*corev1alpha1.Instance)
			return okOld && okNew && !equality.Semantic.DeepEqual(sourcesOf(oldIn), sourcesOf(newIn))
		},
	}
}

func sourcesOf(in *corev1alpha1.Instance) *corev1alpha1.MonitoringSources {
	if in.Status.Monitoring == nil {
		return nil
	}
	return in.Status.Monitoring.Sources
}

func (r *bindingReconciler) bindingsForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	in, ok := obj.(*corev1alpha1.Instance)
	if !ok {
		return nil
	}
	bindings, err := monitoring.ListBindingsForInstance(ctx, r.client, in)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing bindings for Instance failed", "instance", in.Name)
		return nil
	}
	var reqs []reconcile.Request
	for i := range bindings {
		if r.serves(&bindings[i]) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&bindings[i])})
		}
	}
	return reqs
}

func (r *bindingReconciler) bindingsForDestination(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &monitoringv1alpha1.MonitoringBindingList{}
	if err := r.client.List(ctx, list, client.InNamespace(obj.GetNamespace()), client.MatchingLabels{
		monitoringv1alpha1.BindingDestinationLabel: obj.GetName(),
		monitoringv1alpha1.BindingClassLabel:       r.className,
	}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing bindings for MonitoringDestination failed", "destination", obj.GetName())
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if r.serves(&list.Items[i]) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

func labelsString(l map[string]string) string {
	if len(l) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(l))
	for _, k := range slices.Sorted(maps.Keys(l)) {
		parts = append(parts, k+"="+l[k])
	}
	return strings.Join(parts, ",")
}

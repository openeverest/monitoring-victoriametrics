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

// ControllerName is matched against MonitoringClass.spec.controllerName and
// is the field owner of everything this controller writes.
const ControllerName = "openeverest.io/monitoring-victoriametrics"

// applyRetryInterval paces retries while VMPodScrapes cannot be written, for
// example until the VictoriaMetrics Operator CRDs are installed.
const applyRetryInterval = time.Minute

// Setup registers the controllers serving the MonitoringClass className: one
// claims the class and one renders the VMPodScrapes of every binding.
// VMPodScrapes are only ever written, never watched, so the manager starts
// without the VictoriaMetrics Operator CRDs.
func Setup(mgr ctrl.Manager, className string) error {
	cc := monitoring.ClassController{Client: mgr.GetClient(), ControllerName: ControllerName}

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-victoriametrics-class").
		For(&monitoringv1alpha1.MonitoringClass{}, builder.WithPredicates(
			predicate.GenerationChangedPredicate{},
			predicate.NewPredicateFuncs(func(obj client.Object) bool {
				class, ok := obj.(*monitoringv1alpha1.MonitoringClass)
				return ok && class.Spec.ControllerName == ControllerName
			}),
		)).
		Complete(&classReconciler{client: mgr.GetClient(), cc: cc}); err != nil {
		return fmt.Errorf("set up MonitoringClass controller: %w", err)
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

// classReconciler claims every MonitoringClass naming this controller.
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

// sourcesChanged passes Instance events that can change
// status.monitoring.sources, the only Instance field VMPodScrapes derive
// from.
func sourcesChanged() predicate.Funcs {
	return predicate.Funcs{
		DeleteFunc: func(event.DeleteEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldIn, okOld := e.ObjectOld.(*corev1alpha1.Instance)
			newIn, okNew := e.ObjectNew.(*corev1alpha1.Instance)
			return !okOld || !okNew || !equality.Semantic.DeepEqual(sourcesOf(oldIn), sourcesOf(newIn))
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
	return r.servedBindings(ctx, obj.GetNamespace(), client.MatchingLabels{monitoringv1alpha1.BindingInstanceLabel: obj.GetName()})
}

func (r *bindingReconciler) bindingsForDestination(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.servedBindings(ctx, obj.GetNamespace(), client.MatchingLabels{monitoringv1alpha1.BindingDestinationLabel: obj.GetName()})
}

func (r *bindingReconciler) servedBindings(ctx context.Context, namespace string, selector client.MatchingLabels) []reconcile.Request {
	list := &monitoringv1alpha1.MonitoringBindingList{}
	if err := r.client.List(ctx, list, client.InNamespace(namespace), selector); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing bindings failed")
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

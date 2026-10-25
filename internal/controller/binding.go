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

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

// bindingReconciler turns one binding into VMPodScrapes.
type bindingReconciler struct {
	client    client.Client
	cc        monitoring.ClassController
	className string
}

func (r *bindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	b := &monitoringv1alpha1.MonitoringBinding{}
	if err := r.client.Get(ctx, req.NamespacedName, b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !b.DeletionTimestamp.IsZero() || !r.serves(b) {
		return ctrl.Result{}, nil
	}
	ours, err := r.cc.OwnsClass(ctx, r.className)
	if err != nil || !ours {
		return ctrl.Result{}, err
	}
	if !monitoring.IsAccepted(b) {
		return ctrl.Result{}, r.reportNotAccepted(ctx, b)
	}

	in, dst, err := r.cc.Resolve(ctx, b)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonRenderFailed, err.Error())
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	params, err := decodeParameters(dst.Spec.Parameters)
	if err != nil {
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonRenderFailed,
			fmt.Sprintf("MonitoringDestination %s: %v", dst.Name, err))
	}
	sources := sourcesOf(in)
	if sources == nil || len(sources.Metrics) == 0 {
		if err := r.pruneStale(ctx, b, nil); err != nil {
			return r.applyFailed(ctx, b, err)
		}
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonEndpointNotServing,
			fmt.Sprintf("Instance %s publishes no metrics endpoints yet", in.Name))
	}
	scrapes, err := vmPodScrapesFor(b, r.className, sources, params)
	if err != nil {
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonRenderFailed, err.Error())
	}

	names, err := r.applyVMPodScrapes(ctx, b, scrapes)
	if err != nil {
		return r.applyFailed(ctx, b, err)
	}
	return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonConfigured, "VMPodScrapes "+strings.Join(names, ", "))
}

// serves reports whether b is an ExtensionManaged binding of our class; any
// other binding has a different Configured writer.
func (r *bindingReconciler) serves(b *monitoringv1alpha1.MonitoringBinding) bool {
	return b.Spec.ExecutionMode == corev1alpha1.MonitoringExecutionModeExtensionManaged &&
		b.Labels[monitoringv1alpha1.BindingClassLabel] == r.className
}

// applyVMPodScrapes writes the rendered VMPodScrapes, deletes the binding's
// VMPodScrapes for components the Instance no longer publishes and returns
// the sorted names of the live ones.
func (r *bindingReconciler) applyVMPodScrapes(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding, scrapes []*vmPodScrape) ([]string, error) {
	names := make([]string, 0, len(scrapes))
	for _, s := range scrapes {
		u, err := s.toUnstructured()
		if err != nil {
			return nil, err
		}
		if err := r.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(ControllerName), client.ForceOwnership); err != nil {
			return nil, fmt.Errorf("apply VMPodScrape %s: %w", s.Name, err)
		}
		names = append(names, s.Name)
	}
	if err := r.pruneStale(ctx, b, names); err != nil {
		return nil, err
	}
	slices.Sort(names)
	return names, nil
}

// applyFailed reports a rejected write and retries it, so bindings recover
// once the VictoriaMetrics Operator CRDs are installed.
func (r *bindingReconciler) applyFailed(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding, err error) (ctrl.Result, error) {
	message := err.Error()
	if meta.IsNoMatchError(err) {
		message = "the VictoriaMetrics Operator CRDs are not installed: " + message
	}
	if err := r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonApplyFailed, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: applyRetryInterval}, nil
}

// reportNotAccepted reports a binding core has not accepted. Its
// VMPodScrapes are kept until core accepts it again or deletes it.
func (r *bindingReconciler) reportNotAccepted(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding) error {
	live, err := r.ownedVMPodScrapes(ctx, b)
	if err != nil {
		return err
	}
	reason, message := notAcceptedReason(b, len(live) > 0)
	return r.cc.SetConfigured(ctx, b, reason, message)
}

// notAcceptedReason picks the Configured reason of a binding core has not
// accepted: Retained while VMPodScrapes the binding owns are kept,
// NotAccepted when core rejected it, Pending while core has not decided.
func notAcceptedReason(b *monitoringv1alpha1.MonitoringBinding, retained bool) (monitoringv1alpha1.ConfiguredReason, string) {
	accepted := meta.FindStatusCondition(b.Status.Conditions, monitoringv1alpha1.MonitoringBindingConditionAccepted)
	decided := accepted != nil && accepted.ObservedGeneration == b.Generation
	acceptedReason := "not evaluated yet"
	if decided {
		acceptedReason = accepted.Reason
		if accepted.Message != "" {
			acceptedReason += ": " + accepted.Message
		}
	}
	switch {
	case retained:
		return monitoringv1alpha1.ReasonRetained, "keeping the VMPodScrapes while the binding is not accepted (" + acceptedReason + ")"
	case decided && accepted.Status == metav1.ConditionFalse:
		return monitoringv1alpha1.ReasonNotAccepted, "binding not accepted: " + acceptedReason
	default:
		return monitoringv1alpha1.ReasonPending, "waiting for the binding to be accepted"
	}
}

// ownedVMPodScrapes lists the VMPodScrapes the binding controls. There are
// none while the VictoriaMetrics Operator CRDs are not installed.
func (r *bindingReconciler) ownedVMPodScrapes(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(vmPodScrapeAPIVersion)
	list.SetKind(vmPodScrapeKind + "List")
	err := r.client.List(ctx, list, client.InNamespace(b.Namespace), client.MatchingLabels{
		monitoringv1alpha1.BindingInstanceLabel: b.Spec.InstanceRef.Name,
		monitoringv1alpha1.BindingClassLabel:    r.className,
	})
	if meta.IsNoMatchError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list VMPodScrapes: %w", err)
	}
	return slices.DeleteFunc(list.Items, func(s unstructured.Unstructured) bool {
		return !metav1.IsControlledBy(&s, b)
	}), nil
}

// pruneStale deletes the binding's VMPodScrapes that are not in keep.
func (r *bindingReconciler) pruneStale(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding, keep []string) error {
	live, err := r.ownedVMPodScrapes(ctx, b)
	if err != nil {
		return err
	}
	for i := range live {
		if slices.Contains(keep, live[i].GetName()) {
			continue
		}
		if err := r.client.Delete(ctx, &live[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete stale VMPodScrape %s: %w", live[i].GetName(), err)
		}
	}
	return nil
}

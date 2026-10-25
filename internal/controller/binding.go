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
	"maps"
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
	if !monitoring.IsAccepted(b) {
		reason, message := notAcceptedReason(b)
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, reason, message)
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
	if err := r.cc.SetDestinationReady(ctx, dst, monitoringv1alpha1.ReasonReady, readyMessage(params), ""); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "writing destination Ready failed", "destination", dst.Name)
	}
	return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonConfigured, "VMPodScrapes "+strings.Join(names, ", "))
}

// serves reports whether b is an ExtensionManaged binding of our class.
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

// notAcceptedReason picks the Configured reason of a binding core has not
// accepted: Retained while VMPodScrapes from an earlier acceptance are kept,
// NotAccepted when core rejected it, Pending while core has not decided.
func notAcceptedReason(b *monitoringv1alpha1.MonitoringBinding) (monitoringv1alpha1.ConfiguredReason, string) {
	accepted := meta.FindStatusCondition(b.Status.Conditions, monitoringv1alpha1.MonitoringBindingConditionAccepted)
	acceptedReason := "Pending"
	if accepted != nil && accepted.Status == metav1.ConditionFalse {
		acceptedReason = accepted.Reason
	}
	configured := meta.FindStatusCondition(b.Status.Conditions, monitoringv1alpha1.MonitoringBindingConditionConfigured)
	switch {
	case configured != nil && (configured.Status == metav1.ConditionTrue || configured.Reason == string(monitoringv1alpha1.ReasonRetained)):
		return monitoringv1alpha1.ReasonRetained, "keeping the current VMPodScrapes while the binding is not accepted: " + acceptedReason
	case accepted != nil && accepted.Status == metav1.ConditionFalse:
		return monitoringv1alpha1.ReasonNotAccepted, "the binding is not accepted: " + acceptedReason
	default:
		return monitoringv1alpha1.ReasonPending, "waiting for the binding to be accepted"
	}
}

// pruneStale deletes the binding's VMPodScrapes for components the Instance
// no longer publishes.
func (r *bindingReconciler) pruneStale(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding, keep []string) error {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(vmPodScrapeAPIVersion)
	list.SetKind(vmPodScrapeKind + "List")
	if err := r.client.List(ctx, list, client.InNamespace(b.Namespace), client.MatchingLabels{
		monitoringv1alpha1.BindingInstanceLabel: b.Spec.InstanceRef.Name,
		monitoringv1alpha1.BindingClassLabel:    r.className,
	}); err != nil {
		return fmt.Errorf("list VMPodScrapes: %w", err)
	}
	for i := range list.Items {
		s := &list.Items[i]
		owner := metav1.GetControllerOf(s)
		if owner == nil || owner.UID != b.UID || slices.Contains(keep, s.GetName()) {
			continue
		}
		if err := r.client.Delete(ctx, s); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete stale VMPodScrape %s: %w", s.GetName(), err)
		}
	}
	return nil
}

func readyMessage(params destinationParameters) string {
	if len(params.VMPodScrapeLabels) == 0 {
		return "VMPodScrapes carry no selection labels"
	}
	parts := make([]string, 0, len(params.VMPodScrapeLabels))
	for _, k := range slices.Sorted(maps.Keys(params.VMPodScrapeLabels)) {
		parts = append(parts, k+"="+params.VMPodScrapeLabels[k])
	}
	return "VMPodScrapes are labelled " + strings.Join(parts, ",")
}

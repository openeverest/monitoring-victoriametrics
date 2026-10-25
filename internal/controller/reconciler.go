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

// Package controller reconciles ExtensionManaged MonitoringBindings of the
// victoriametrics class into VictoriaMetrics Operator VMPodScrapes.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

// ControllerName is matched against MonitoringClass.spec.controllerName.
const ControllerName = "openeverest.io/monitoring-victoriametrics"

// ClassName is the MonitoringClass this PoC controller claims. A real
// extension would claim every class carrying ControllerName.
const ClassName = "victoriametrics"

var vmPodScrapeGVK = schema.GroupVersionKind{Group: "operator.victoriametrics.com", Version: "v1beta1", Kind: "VMPodScrape"}

// destinationParameters is MonitoringDestination.spec.parameters for the
// victoriametrics class: the labels the target VMAgent selects VMPodScrapes by.
type destinationParameters struct {
	VMPodScrapeLabels map[string]string `json:"vmPodScrapeLabels,omitempty"`
	Interval          string            `json:"interval,omitempty"`
}

// Reconciler turns one binding into VMPodScrapes.
type Reconciler struct {
	client.Client
	cc monitoring.ClassController
}

// Setup wires the controller: bindings of our class are the primary
// resource; Instances (status.monitoring.sources changes) and the class (claim) are
// mapped back onto bindings.
func Setup(mgr ctrl.Manager) error {
	r := &Reconciler{Client: mgr.GetClient(), cc: monitoring.ClassController{Client: mgr.GetClient(), ControllerName: ControllerName}}
	return ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-victoriametrics").
		For(&monitoringv1alpha1.MonitoringBinding{}, builder.WithPredicates(monitoring.BindingPredicate(ClassName))).
		Watches(&corev1alpha1.Instance{}, handler.EnqueueRequestsFromMapFunc(r.bindingsForInstance)).
		Watches(&monitoringv1alpha1.MonitoringClass{}, handler.EnqueueRequestsFromMapFunc(r.claim),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

// claim sets Accepted on our class whenever it appears or changes; nothing to
// enqueue since bindings of an unclaimed class are not Accepted anyway and
// core re-resolves them once the class is claimed.
func (r *Reconciler) claim(ctx context.Context, _ client.Object) []reconcile.Request {
	if _, err := r.cc.Claim(ctx); err != nil {
		log.FromContext(ctx).Error(err, "claiming classes failed")
	}
	return nil
}

func (r *Reconciler) bindingsForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	bindings, err := monitoring.ListBindingsForInstance(ctx, r.Client, obj.GetNamespace(), obj.GetName())
	if err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range bindings {
		if bindings[i].Labels[monitoringv1alpha1.BindingClassLabel] == ClassName {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&bindings[i])})
		}
	}
	return reqs
}

// Reconcile renders the VMPodScrapes of one binding.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	b := &monitoringv1alpha1.MonitoringBinding{}
	if err := r.Get(ctx, req.NamespacedName, b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !b.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // VMPodScrapes are owned by the binding
	}
	if !monitoring.IsAccepted(b) {
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, notAcceptedReason(b), "waiting for core to accept the binding")
	}

	in, dst, err := r.cc.Resolve(ctx, b)
	if err != nil {
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonRenderFailed, err.Error())
	}
	params := destinationParameters{}
	if dst.Spec.Parameters != nil {
		if err := json.Unmarshal(dst.Spec.Parameters.Raw, &params); err != nil {
			return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonRenderFailed, "destination parameters: "+err.Error())
		}
	}
	if err := r.cc.SetDestinationReady(ctx, dst, metav1.ConditionTrue, "Configured", "VMPodScrapes are selected by labels "+labelsString(params.VMPodScrapeLabels), ""); err != nil {
		logger.Error(err, "destination Ready could not be written")
	}

	if in.Status.Monitoring == nil || in.Status.Monitoring.Sources == nil || len(in.Status.Monitoring.Sources.Metrics) == 0 {
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonEndpointNotServing, "the Instance publishes no metrics endpoints yet")
	}

	desired := map[string]struct{}{}
	for _, pm := range vmPodScrapesFor(b, in, in.Status.Monitoring.Sources, params) {
		desired[pm.GetName()] = struct{}{}
		if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(pm), client.FieldOwner(ControllerName), client.ForceOwnership); err != nil {
			return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonApplyFailed, fmt.Sprintf("apply VMPodScrape %s: %v", pm.GetName(), err))
		}
	}
	if err := r.pruneStale(ctx, b, desired); err != nil {
		logger.Error(err, "pruning stale VMPodScrapes failed")
	}

	names := make([]string, 0, len(desired))
	for n := range desired {
		names = append(names, n)
	}
	sort.Strings(names)
	return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonConfigured,
		fmt.Sprintf("VMPodScrapes %s", strings.Join(names, ", ")))
}

// notAcceptedReason picks the Configured reason for a binding that is not
// Accepted: Pending while core has not decided, Retained when VMPodScrapes
// from an earlier acceptance are kept, NotAccepted otherwise.
func notAcceptedReason(b *monitoringv1alpha1.MonitoringBinding) monitoringv1alpha1.ConfiguredReason {
	accepted := meta.FindStatusCondition(b.Status.Conditions, monitoringv1alpha1.MonitoringBindingConditionAccepted)
	if accepted == nil || accepted.Status == metav1.ConditionUnknown {
		return monitoringv1alpha1.ReasonPending
	}
	prev := meta.FindStatusCondition(b.Status.Conditions, monitoringv1alpha1.MonitoringBindingConditionConfigured)
	if prev != nil && (prev.Status == metav1.ConditionTrue || prev.Reason == string(monitoringv1alpha1.ReasonRetained)) {
		return monitoringv1alpha1.ReasonRetained
	}
	return monitoringv1alpha1.ReasonNotAccepted
}

// vmPodScrapesFor builds one VMPodScrape per component, owned by the binding,
// with one podMetricsEndpoints entry per metrics endpoint of that component.
// A VMPodScrape has a single pod selector, so endpoints of one component must
// share theirs; a multi-target exporter publishes one endpoint per probed
// pod behind the same selector.
func vmPodScrapesFor(b *monitoringv1alpha1.MonitoringBinding, in *corev1alpha1.Instance, sources *corev1alpha1.MonitoringSources, params destinationParameters) []*unstructured.Unstructured {
	var order []string
	byComponent := map[string]*unstructured.Unstructured{}
	for _, ep := range sources.Metrics {
		pm, ok := byComponent[ep.Component]
		if !ok {
			pm = vmPodScrapeFor(b, in, ep, params)
			byComponent[ep.Component] = pm
			order = append(order, ep.Component)
		}
		endpoints, _, _ := unstructured.NestedSlice(pm.Object, "spec", "podMetricsEndpoints")
		endpoints = append(endpoints, vmPodMetricsEndpointFor(sources, ep, params))
		_ = unstructured.SetNestedSlice(pm.Object, endpoints, "spec", "podMetricsEndpoints")
	}
	out := make([]*unstructured.Unstructured, 0, len(order))
	for _, component := range order {
		out = append(out, byComponent[component])
	}
	return out
}

// vmPodMetricsEndpointFor renders one scrape endpoint, stamped with the identity
// labels through relabelings. A multi-target probe (params.target) relabels
// the target into the instance label so each probed pod is its own series.
func vmPodMetricsEndpointFor(sources *corev1alpha1.MonitoringSources, ep corev1alpha1.MetricsEndpoint, params destinationParameters) map[string]any {
	endpoint := map[string]any{
		"path":   ep.Path,
		"scheme": ep.Scheme,
	}
	if endpoint["path"] == "" {
		endpoint["path"] = "/metrics"
	}
	if endpoint["scheme"] == "" {
		endpoint["scheme"] = "http"
	}
	if ep.Port.Name != "" {
		endpoint["port"] = ep.Port.Name
	} else if ep.Port.Number != nil {
		endpoint["portNumber"] = int64(*ep.Port.Number)
	}
	if params.Interval != "" {
		endpoint["interval"] = params.Interval
	}
	if ep.TLS != nil {
		tls := map[string]any{"insecureSkipVerify": ep.TLS.InsecureSkipVerify}
		if ep.TLS.ServerName != "" {
			tls["serverName"] = ep.TLS.ServerName
		}
		if ep.TLS.CASecretRef != nil {
			tls["ca"] = map[string]any{"secret": map[string]any{"name": ep.TLS.CASecretRef.Name, "key": ep.TLS.CASecretRef.Key}}
		}
		endpoint["tlsConfig"] = tls
	}
	if ep.Auth != nil {
		for _, cred := range sources.Credentials {
			if cred.Profile == ep.Auth.CredentialProfile {
				endpoint["basicAuth"] = map[string]any{
					"username": map[string]any{"name": cred.SecretRef.Name, "key": "username"},
					"password": map[string]any{"name": cred.SecretRef.Name, "key": "password"},
				}
			}
		}
	}
	if len(ep.Params) > 0 {
		p := map[string]any{}
		for k, v := range ep.Params {
			p[k] = []any{v}
		}
		endpoint["params"] = p
	}

	identity := map[string]string{}
	for k, v := range sources.Identity.Labels {
		identity[k] = v
	}
	identity["openeverest_component"] = ep.Component
	identity["openeverest_kind"] = ep.Kind
	keys := make([]string, 0, len(identity))
	for k := range identity {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	relabelings := make([]any, 0, len(keys))
	for _, k := range keys {
		relabelings = append(relabelings, map[string]any{"targetLabel": k, "replacement": identity[k], "action": "replace"})
	}
	if _, ok := ep.Params["target"]; ok {
		relabelings = append(relabelings, map[string]any{"sourceLabels": []any{"__param_target"}, "targetLabel": "instance", "action": "replace"})
	}
	endpoint["relabelConfigs"] = relabelings
	return endpoint
}

// vmPodScrapeFor builds the VMPodScrape shell for one component: selector,
// labels and owner, with no endpoints yet.
func vmPodScrapeFor(b *monitoringv1alpha1.MonitoringBinding, in *corev1alpha1.Instance, ep corev1alpha1.MetricsEndpoint, params destinationParameters) *unstructured.Unstructured {
	selector := map[string]any{}
	for k, v := range ep.PodSelector {
		selector[k] = v
	}
	labels := map[string]any{
		monitoringv1alpha1.BindingInstanceLabel: in.Name,
		monitoringv1alpha1.BindingClassLabel:    ClassName,
	}
	for k, v := range params.VMPodScrapeLabels {
		labels[k] = v
	}
	owner := monitoring.OwnedBy(b)

	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": vmPodScrapeGVK.GroupVersion().String(),
		"kind":       vmPodScrapeGVK.Kind,
		"metadata": map[string]any{
			"name":      b.Name + "-" + strings.ToLower(ep.Component), // component names are not DNS labels
			"namespace": b.Namespace,
			"labels":    labels,
			"ownerReferences": []any{map[string]any{
				"apiVersion":         owner.APIVersion,
				"kind":               owner.Kind,
				"name":               owner.Name,
				"uid":                string(owner.UID),
				"controller":         true,
				"blockOwnerDeletion": true,
			}},
		},
		"spec": map[string]any{
			"selector":            map[string]any{"matchLabels": selector},
			"namespaceSelector":   map[string]any{"matchNames": []any{b.Namespace}},
			"podMetricsEndpoints": []any{},
		},
	}}
	return u
}

// pruneStale deletes VMPodScrapes owned by the binding whose endpoint is gone.
func (r *Reconciler) pruneStale(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding, desired map[string]struct{}) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(vmPodScrapeGVK.GroupVersion().WithKind("VMPodScrapeList"))
	if err := r.List(ctx, list, client.InNamespace(b.Namespace), client.MatchingLabels{monitoringv1alpha1.BindingInstanceLabel: b.Spec.InstanceRef.Name}); err != nil {
		return err
	}
	for i := range list.Items {
		pm := &list.Items[i]
		if _, keep := desired[pm.GetName()]; keep || metav1.GetControllerOf(pm) == nil || metav1.GetControllerOf(pm).UID != b.UID {
			continue
		}
		if err := r.Delete(ctx, pm); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func labelsString(l map[string]string) string {
	if len(l) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(l))
	for k, v := range l {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

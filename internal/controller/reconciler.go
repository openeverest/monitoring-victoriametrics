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
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
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

const (
	vmPodScrapeAPIVersion = "operator.victoriametrics.com/v1beta1"
	vmPodScrapeKind       = "VMPodScrape"
)

// applyRetryInterval paces retries of rejected writes, so bindings recover
// once the VictoriaMetrics Operator CRDs are installed.
const applyRetryInterval = time.Minute

// destinationParameters is MonitoringDestination.spec.parameters of the
// victoriametrics class.
type destinationParameters struct {
	VMPodScrapeLabels map[string]string `json:"vmPodScrapeLabels,omitempty"`
	Interval          string            `json:"interval,omitempty"`
}

// Reconciler turns one binding into VMPodScrapes.
type Reconciler struct {
	client.Client

	cc         monitoring.ClassController
	className  string
	ownBinding predicate.Predicate
}

// NewReconciler returns a Reconciler for the bindings of the named class.
func NewReconciler(c client.Client, className string) *Reconciler {
	return &Reconciler{
		Client:     c,
		cc:         monitoring.ClassController{Client: c, ControllerName: ControllerName},
		className:  className,
		ownBinding: monitoring.BindingPredicate(className),
	}
}

// Setup wires two controllers: one claims the MonitoringClasses naming
// ControllerName, the other renders the bindings of className and is
// re-triggered by changes to an Instance's monitoring sources and to a
// destination's spec.
func Setup(mgr ctrl.Manager, className string) error {
	r := NewReconciler(mgr.GetClient(), className)
	ownClass := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		class, ok := obj.(*monitoringv1alpha1.MonitoringClass)
		return ok && class.Spec.ControllerName == ControllerName
	})
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-victoriametrics-class").
		For(&monitoringv1alpha1.MonitoringClass{}, builder.WithPredicates(predicate.GenerationChangedPredicate{}, ownClass)).
		Complete(reconcile.Func(r.claim)); err != nil {
		return fmt.Errorf("set up class controller: %w", err)
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-victoriametrics").
		For(&monitoringv1alpha1.MonitoringBinding{}, builder.WithPredicates(r.ownBinding)).
		Watches(&corev1alpha1.Instance{}, handler.EnqueueRequestsFromMapFunc(r.bindingsForInstance),
			builder.WithPredicates(sourcesChanged())).
		Watches(&monitoringv1alpha1.MonitoringDestination{}, handler.EnqueueRequestsFromMapFunc(r.bindingsForDestination),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r); err != nil {
		return fmt.Errorf("set up binding controller: %w", err)
	}
	return nil
}

// sourcesChanged passes Instance events that can change what is scraped.
func sourcesChanged() predicate.Predicate { //nolint:ireturn // controller-runtime's builder takes the interface
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldIn, okOld := e.ObjectOld.(*corev1alpha1.Instance)
			newIn, okNew := e.ObjectNew.(*corev1alpha1.Instance)
			return !okOld || !okNew || !equality.Semantic.DeepEqual(sourcesOf(oldIn), sourcesOf(newIn))
		},
		DeleteFunc: func(event.DeleteEvent) bool { return false },
	}
}

// Reconcile renders the VMPodScrapes of one binding and reports the outcome
// in the binding's Configured condition.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	b := &monitoringv1alpha1.MonitoringBinding{}
	if err := r.Get(ctx, req.NamespacedName, b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !b.DeletionTimestamp.IsZero() || !r.owns(b) {
		return ctrl.Result{}, nil // VMPodScrapes are owned by the binding
	}
	if !monitoring.IsAccepted(b) {
		reason, message := notAcceptedReason(b)
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, reason, message)
	}

	in, dst, err := r.cc.Resolve(ctx, b)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonRenderFailed, err.Error())
	}
	params, err := parametersOf(dst)
	if err != nil {
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonRenderFailed, err.Error())
	}
	sources := sourcesOf(in)
	if sources == nil || len(sources.Metrics) == 0 {
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonEndpointNotServing, "the Instance publishes no metrics endpoints yet")
	}
	scrapes, err := vmPodScrapesFor(b, r.className, sources, params)
	if err != nil {
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonRenderFailed, err.Error())
	}
	names, err := r.applyAll(ctx, b, scrapes)
	if err != nil {
		return r.applyFailed(ctx, b, err)
	}
	if err := r.cc.SetDestinationReady(ctx, dst, monitoringv1alpha1.ReasonReady, readyMessage(params), ""); err != nil {
		log.FromContext(ctx).Error(err, "writing destination Ready failed", "destination", dst.Name)
	}
	return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonConfigured, "VMPodScrapes "+strings.Join(names, ", "))
}

func (r *Reconciler) claim(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	class := &monitoringv1alpha1.MonitoringClass{}
	if err := r.Get(ctx, req.NamespacedName, class); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	return reconcile.Result{}, r.cc.ClaimClass(ctx, class)
}

func (r *Reconciler) owns(b *monitoringv1alpha1.MonitoringBinding) bool {
	return r.ownBinding.Generic(event.GenericEvent{Object: b})
}

func (r *Reconciler) bindingsForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.ownBindings(ctx, obj.GetNamespace(), client.MatchingLabels{monitoringv1alpha1.BindingInstanceLabel: obj.GetName()})
}

func (r *Reconciler) bindingsForDestination(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.ownBindings(ctx, obj.GetNamespace(), client.MatchingLabels{monitoringv1alpha1.BindingDestinationLabel: obj.GetName()})
}

func (r *Reconciler) ownBindings(ctx context.Context, namespace string, selector client.MatchingLabels) []reconcile.Request {
	list := &monitoringv1alpha1.MonitoringBindingList{}
	if err := r.List(ctx, list, client.InNamespace(namespace), selector); err != nil {
		log.FromContext(ctx).Error(err, "listing monitoring bindings failed")
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if r.owns(&list.Items[i]) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

// applyAll applies the binding's VMPodScrapes, deletes the ones of
// components that no longer publish metrics and returns the sorted names.
func (r *Reconciler) applyAll(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding, scrapes []*unstructured.Unstructured) ([]string, error) {
	names := make([]string, 0, len(scrapes))
	for _, s := range scrapes {
		if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(s), client.FieldOwner(ControllerName), client.ForceOwnership); err != nil {
			return nil, fmt.Errorf("apply VMPodScrape %s: %w", s.GetName(), err)
		}
		names = append(names, s.GetName())
	}
	slices.Sort(names)

	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(vmPodScrapeAPIVersion)
	list.SetKind(vmPodScrapeKind + "List")
	if err := r.List(ctx, list, client.InNamespace(b.Namespace),
		client.MatchingLabels{monitoringv1alpha1.BindingInstanceLabel: b.Spec.InstanceRef.Name}); err != nil {
		return nil, fmt.Errorf("list VMPodScrapes: %w", err)
	}
	var errs []error
	for i := range list.Items {
		s := &list.Items[i]
		owner := metav1.GetControllerOf(s)
		if owner == nil || owner.UID != b.UID || slices.Contains(names, s.GetName()) {
			continue
		}
		if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete VMPodScrape %s: %w", s.GetName(), err))
		}
	}
	return names, errors.Join(errs...)
}

func (r *Reconciler) applyFailed(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding, err error) (ctrl.Result, error) {
	message := err.Error()
	if meta.IsNoMatchError(err) {
		message = "the VictoriaMetrics Operator CRDs are not installed: " + message
	}
	if err := r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonApplyFailed, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: applyRetryInterval}, nil
}

func parametersOf(dst *monitoringv1alpha1.MonitoringDestination) (destinationParameters, error) {
	params := destinationParameters{}
	if dst.Spec.Parameters == nil || len(dst.Spec.Parameters.Raw) == 0 {
		return params, nil
	}
	if err := json.Unmarshal(dst.Spec.Parameters.Raw, &params); err != nil {
		return params, fmt.Errorf("destination parameters: %w", err)
	}
	return params, nil
}

func sourcesOf(in *corev1alpha1.Instance) *corev1alpha1.MonitoringSources {
	if in.Status.Monitoring == nil {
		return nil
	}
	return in.Status.Monitoring.Sources
}

// notAcceptedReason picks the Configured reason of a binding that is not
// Accepted: Retained while VMPodScrapes from an earlier acceptance are kept,
// NotAccepted once core rejected it, Pending while core has not decided.
func notAcceptedReason(b *monitoringv1alpha1.MonitoringBinding) (monitoringv1alpha1.ConfiguredReason, string) {
	accepted := meta.FindStatusCondition(b.Status.Conditions, monitoringv1alpha1.MonitoringBindingConditionAccepted)
	acceptedReason := "Pending"
	if accepted != nil && accepted.Status == metav1.ConditionFalse {
		acceptedReason = accepted.Reason
	}
	prev := meta.FindStatusCondition(b.Status.Conditions, monitoringv1alpha1.MonitoringBindingConditionConfigured)
	switch {
	case prev != nil && (prev.Status == metav1.ConditionTrue || prev.Reason == string(monitoringv1alpha1.ReasonRetained)):
		return monitoringv1alpha1.ReasonRetained, "keeping the current VMPodScrapes while the binding is not accepted: " + acceptedReason
	case accepted != nil && accepted.Status == metav1.ConditionFalse:
		return monitoringv1alpha1.ReasonNotAccepted, "the binding is not accepted: " + acceptedReason
	default:
		return monitoringv1alpha1.ReasonPending, "waiting for the binding to be accepted"
	}
}

func readyMessage(params destinationParameters) string {
	if len(params.VMPodScrapeLabels) == 0 {
		return "VMPodScrapes carry no selection labels"
	}
	parts := make([]string, 0, len(params.VMPodScrapeLabels))
	for k, v := range params.VMPodScrapeLabels {
		parts = append(parts, k+"="+v)
	}
	slices.Sort(parts)
	return "VMPodScrapes are labelled " + strings.Join(parts, ",")
}

// vmPodScrapesFor builds one VMPodScrape per component, with one
// podMetricsEndpoints entry per metrics endpoint of that component. A
// VMPodScrape has a single pod selector, so the endpoints of one component
// must share theirs.
func vmPodScrapesFor(b *monitoringv1alpha1.MonitoringBinding, className string, sources *corev1alpha1.MonitoringSources, params destinationParameters) ([]*unstructured.Unstructured, error) {
	byComponent := map[string][]corev1alpha1.MetricsEndpoint{}
	for _, ep := range sources.Metrics {
		byComponent[ep.Component] = append(byComponent[ep.Component], ep)
	}
	scrapes := make([]*unstructured.Unstructured, 0, len(byComponent))
	for _, component := range slices.Sorted(maps.Keys(byComponent)) {
		eps := byComponent[component]
		endpoints := make([]any, 0, len(eps))
		for _, ep := range eps {
			if !maps.Equal(ep.PodSelector, eps[0].PodSelector) {
				return nil, fmt.Errorf("component %s publishes metrics endpoints with different pod selectors", component)
			}
			endpoint, err := vmPodMetricsEndpointFor(sources, ep, params)
			if err != nil {
				return nil, err
			}
			endpoints = append(endpoints, endpoint)
		}
		scrapes = append(scrapes, vmPodScrapeFor(b, className, component, eps[0].PodSelector, endpoints, params))
	}
	return scrapes, nil
}

// vmPodMetricsEndpointFor renders one scrape endpoint, stamped with the
// identity labels through relabelConfigs.
func vmPodMetricsEndpointFor(sources *corev1alpha1.MonitoringSources, ep corev1alpha1.MetricsEndpoint, params destinationParameters) (map[string]any, error) {
	endpoint := map[string]any{}
	switch ep.Port.Type {
	case intstr.String:
		endpoint["port"] = ep.Port.StrVal
	case intstr.Int:
		endpoint["portNumber"] = int64(ep.Port.IntVal)
	}
	if ep.Path != "" {
		endpoint["path"] = ep.Path
	}
	if ep.Scheme != "" {
		endpoint["scheme"] = ep.Scheme
	}
	if params.Interval != "" {
		endpoint["interval"] = params.Interval
	}
	if len(ep.Params) > 0 {
		p := make(map[string]any, len(ep.Params))
		for k, values := range ep.Params {
			p[k] = stringsToAny(values)
		}
		endpoint["params"] = p
	}
	if ep.TLS != nil {
		endpoint["tlsConfig"] = tlsConfigFor(ep.TLS)
	}
	if ep.Auth != nil {
		idx := slices.IndexFunc(sources.Credentials, func(c corev1alpha1.MonitoringCredential) bool {
			return c.Profile == ep.Auth.CredentialProfile
		})
		if idx < 0 {
			return nil, fmt.Errorf("component %s authenticates with credential profile %q, which the Instance does not publish",
				ep.Component, ep.Auth.CredentialProfile)
		}
		secret := sources.Credentials[idx].SecretRef.Name
		endpoint["basicAuth"] = map[string]any{
			"username": secretKeySelector(secret, "username"),
			"password": secretKeySelector(secret, "password"),
		}
	}
	endpoint["relabelConfigs"] = relabelConfigsFor(sources.Identity.Labels, ep)
	return endpoint, nil
}

func tlsConfigFor(tls *corev1alpha1.MetricsEndpointTLS) map[string]any {
	cfg := map[string]any{}
	if tls.CA != nil {
		cfg["ca"] = map[string]any{"secret": secretKeySelector(tls.CA.Name, tls.CA.Key)}
	}
	if tls.ServerName != "" {
		cfg["serverName"] = tls.ServerName
	}
	if tls.InsecureSkipVerify {
		cfg["insecureSkipVerify"] = true
	}
	return cfg
}

func secretKeySelector(name, key string) map[string]any {
	return map[string]any{"name": name, "key": key}
}

// relabelConfigsFor stamps the identity labels plus the endpoint's component
// and kind. A multi-target probe (params.target) also relabels the target
// into the instance label so each probed pod is its own series.
func relabelConfigsFor(identity map[string]string, ep corev1alpha1.MetricsEndpoint) []any {
	labels := make(map[string]string, len(identity)+2)
	maps.Copy(labels, identity)
	labels["openeverest_component"] = ep.Component
	labels["openeverest_kind"] = ep.Kind
	relabelings := make([]any, 0, len(labels)+1)
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		relabelings = append(relabelings, map[string]any{"action": "replace", "targetLabel": k, "replacement": labels[k]})
	}
	if _, ok := ep.Params["target"]; ok {
		relabelings = append(relabelings, map[string]any{"action": "replace", "sourceLabels": []any{"__param_target"}, "targetLabel": "instance"})
	}
	return relabelings
}

// vmPodScrapeFor builds the VMPodScrape of one component, owned by the
// binding and labelled for the target VMAgent's podScrapeSelector.
func vmPodScrapeFor(b *monitoringv1alpha1.MonitoringBinding, className, component string, selector map[string]string, endpoints []any, params destinationParameters) *unstructured.Unstructured {
	labels := make(map[string]any, len(params.VMPodScrapeLabels)+2)
	for k, v := range params.VMPodScrapeLabels {
		labels[k] = v
	}
	labels[monitoringv1alpha1.BindingInstanceLabel] = b.Spec.InstanceRef.Name
	labels[monitoringv1alpha1.BindingClassLabel] = className
	matchLabels := make(map[string]any, len(selector))
	for k, v := range selector {
		matchLabels[k] = v
	}

	scrape := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": vmPodScrapeAPIVersion,
		"kind":       vmPodScrapeKind,
		"metadata": map[string]any{
			"name":      b.Name + "-" + strings.ToLower(component), // component names are not DNS labels
			"namespace": b.Namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"selector":            map[string]any{"matchLabels": matchLabels},
			"namespaceSelector":   map[string]any{"matchNames": []any{b.Namespace}},
			"podMetricsEndpoints": endpoints,
		},
	}}
	scrape.SetOwnerReferences([]metav1.OwnerReference{monitoring.OwnedBy(b)})
	return scrape
}

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

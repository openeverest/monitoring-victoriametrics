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
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/kube-openapi/pkg/validation/spec"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	common "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

const (
	testNamespace   = "team-a"
	testClass       = "victoriametrics"
	testInstance    = "orders"
	testDestination = "vm-prod"
	testBinding     = "orders-vm-1a2b3c4d"
	testBindingUID  = types.UID("binding-uid")
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(s))
	require.NoError(t, monitoringv1alpha1.AddToScheme(s))
	return s
}

func newFakeClientBuilder(t *testing.T, objs ...client.Object) *fake.ClientBuilder {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithTypeConverters(crdTypeConverter(t), managedfields.NewDeducedTypeConverter()).
		WithObjects(objs...).
		WithStatusSubresource(
			&corev1alpha1.Instance{},
			&monitoringv1alpha1.MonitoringClass{},
			&monitoringv1alpha1.MonitoringDestination{},
			&monitoringv1alpha1.MonitoringBinding{},
		)
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client { //nolint:ireturn // the fake is used through the client interface
	t.Helper()
	return newFakeClientBuilder(t, objs...).Build()
}

// crdTypeConverter makes the fake client's server-side apply follow the
// monitoring CRD schemas of the pinned core module (listType=map conditions)
// instead of treating every list as atomic.
func crdTypeConverter(t *testing.T) managedfields.TypeConverter { //nolint:ireturn // the fake client takes the interface
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", "github.com/openeverest/openeverest/v2").Output()
	require.NoError(t, err)
	crds := os.DirFS(filepath.Join(strings.TrimSpace(string(out)), "config", "crd", "bases"))
	models := map[string]*spec.Schema{}
	for _, plural := range []string{"monitoringclasses", "monitoringdestinations", "monitoringbindings"} {
		raw, err := fs.ReadFile(crds, "monitoring.openeverest.io_"+plural+".yaml")
		require.NoError(t, err)
		crd := &apiextensionsv1.CustomResourceDefinition{}
		require.NoError(t, yaml.Unmarshal(raw, crd))
		for _, v := range crd.Spec.Versions {
			js, err := json.Marshal(v.Schema.OpenAPIV3Schema)
			require.NoError(t, err)
			s := &spec.Schema{}
			require.NoError(t, json.Unmarshal(js, s))
			s.AddExtension("x-kubernetes-group-version-kind", []any{
				map[string]any{"group": crd.Spec.Group, "version": v.Name, "kind": crd.Spec.Names.Kind},
			})
			models[crd.Spec.Group+"."+v.Name+"."+crd.Spec.Names.Kind] = s
		}
	}
	tc, err := managedfields.NewTypeConverter(models, false)
	require.NoError(t, err)
	return tc
}

func identity() corev1alpha1.MonitoringIdentity {
	return corev1alpha1.MonitoringIdentity{
		ClusterID: "6f1c",
		Labels: map[string]string{
			"openeverest_instance":  testInstance,
			"openeverest_namespace": testNamespace,
			"openeverest_provider":  "percona-xtradb-cluster",
			"k8s_cluster_id":        "6f1c",
		},
	}
}

func newInstance(sources *corev1alpha1.MonitoringSources) *corev1alpha1.Instance {
	in := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: testInstance, Namespace: testNamespace},
		Spec:       corev1alpha1.InstanceSpec{ProviderRef: common.ObjectRef{Name: "percona-xtradb-cluster"}},
	}
	if sources != nil {
		in.Status.Monitoring = &corev1alpha1.InstanceMonitoringStatus{Sources: sources}
	}
	return in
}

func newDestination(parameters string) *monitoringv1alpha1.MonitoringDestination {
	dst := &monitoringv1alpha1.MonitoringDestination{
		ObjectMeta: metav1.ObjectMeta{Name: testDestination, Namespace: testNamespace, Generation: 1},
		Spec:       monitoringv1alpha1.MonitoringDestinationSpec{ClassRef: common.ObjectRef{Name: testClass}},
	}
	if parameters != "" {
		dst.Spec.Parameters = &runtime.RawExtension{Raw: []byte(parameters)}
	}
	return dst
}

func accepted(status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{
		Type: monitoringv1alpha1.MonitoringBindingConditionAccepted, Status: status, Reason: reason,
		ObservedGeneration: 1, LastTransitionTime: metav1.Now(),
	}
}

func newBinding(conditions ...metav1.Condition) *monitoringv1alpha1.MonitoringBinding {
	if len(conditions) == 0 {
		conditions = []metav1.Condition{accepted(metav1.ConditionTrue, monitoringv1alpha1.ReasonAccepted)}
	}
	return &monitoringv1alpha1.MonitoringBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: testBinding, Namespace: testNamespace, UID: testBindingUID, Generation: 1,
			Labels: monitoring.BindingLabels(testInstance, "vm", testDestination, testClass),
		},
		Spec: monitoringv1alpha1.MonitoringBindingSpec{
			InstanceRef:    common.ObjectRef{Name: testInstance},
			DestinationRef: common.MonitoringDestinationRef{Kind: "MonitoringDestination", Name: testDestination},
			ClassRef:       &common.ObjectRef{Name: testClass},
			ExecutionMode:  corev1alpha1.MonitoringExecutionModeExtensionManaged,
		},
		Status: monitoringv1alpha1.MonitoringBindingStatus{Conditions: conditions},
	}
}

func engineEndpoint(port intstr.IntOrString) corev1alpha1.MetricsEndpoint {
	return corev1alpha1.MetricsEndpoint{
		Component:   "engine",
		Kind:        "mysql",
		PodSelector: map[string]string{"core.openeverest.io/instance": testInstance, "core.openeverest.io/component": "engine"},
		Port:        port,
		Path:        "/metrics",
		Scheme:      "http",
	}
}

func engineSources() *corev1alpha1.MonitoringSources {
	return &corev1alpha1.MonitoringSources{Identity: identity(), Metrics: []corev1alpha1.MetricsEndpoint{engineEndpoint(intstr.FromString("metrics"))}}
}

func reconcileBinding(t *testing.T, c client.Client) ctrl.Result {
	t.Helper()
	res, err := NewReconciler(c, testClass).Reconcile(t.Context(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testBinding},
	})
	require.NoError(t, err)
	return res
}

func configured(t *testing.T, c client.Client) *metav1.Condition {
	t.Helper()
	b := &monitoringv1alpha1.MonitoringBinding{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: testBinding}, b))
	return meta.FindStatusCondition(b.Status.Conditions, monitoringv1alpha1.MonitoringBindingConditionConfigured)
}

func vmPodScrapes(t *testing.T, c client.Client) map[string]*unstructured.Unstructured {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(vmPodScrapeAPIVersion)
	list.SetKind(vmPodScrapeKind + "List")
	require.NoError(t, c.List(t.Context(), list, client.InNamespace(testNamespace)))
	out := map[string]*unstructured.Unstructured{}
	for i := range list.Items {
		out[list.Items[i].GetName()] = &list.Items[i]
	}
	return out
}

func engineRelabelConfigs() []any {
	return []any{
		map[string]any{"action": "replace", "targetLabel": "k8s_cluster_id", "replacement": "6f1c"},
		map[string]any{"action": "replace", "targetLabel": "openeverest_component", "replacement": "engine"},
		map[string]any{"action": "replace", "targetLabel": "openeverest_instance", "replacement": testInstance},
		map[string]any{"action": "replace", "targetLabel": "openeverest_kind", "replacement": "mysql"},
		map[string]any{"action": "replace", "targetLabel": "openeverest_namespace", "replacement": testNamespace},
		map[string]any{"action": "replace", "targetLabel": "openeverest_provider", "replacement": "percona-xtradb-cluster"},
	}
}

func TestReconcileRendersEndpoint(t *testing.T) {
	t.Parallel()

	withTarget := append(engineRelabelConfigs(),
		map[string]any{"action": "replace", "sourceLabels": []any{"__param_target"}, "targetLabel": "instance"})
	tests := []struct {
		name        string
		endpoint    func(ep *corev1alpha1.MetricsEndpoint)
		credentials []corev1alpha1.MonitoringCredential
		parameters  string
		want        map[string]any
	}{
		{
			name: "port by name",
			want: map[string]any{
				"port": "metrics", "path": "/metrics", "scheme": "http",
				"relabelConfigs": engineRelabelConfigs(),
			},
		},
		{
			name:     "port by number",
			endpoint: func(ep *corev1alpha1.MetricsEndpoint) { ep.Port = intstr.FromInt32(9104) },
			want: map[string]any{
				"portNumber": int64(9104), "path": "/metrics", "scheme": "http",
				"relabelConfigs": engineRelabelConfigs(),
			},
		},
		{
			name: "multi-target probe keeps params verbatim and relabels the target into instance",
			endpoint: func(ep *corev1alpha1.MetricsEndpoint) {
				ep.Path = "/probe"
				ep.Params = map[string][]string{"target": {"mysql://127.0.0.1:3306"}, "collect[]": {"global_status", "info_schema"}}
			},
			want: map[string]any{
				"port": "metrics", "path": "/probe", "scheme": "http",
				"params": map[string]any{
					"target":    []any{"mysql://127.0.0.1:3306"},
					"collect[]": []any{"global_status", "info_schema"},
				},
				"relabelConfigs": withTarget,
			},
		},
		{
			name: "tls with a CA secret",
			endpoint: func(ep *corev1alpha1.MetricsEndpoint) {
				ep.Scheme = "https"
				ep.TLS = &corev1alpha1.MetricsEndpointTLS{
					CA:         &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "orders-ca"}, Key: "ca.crt"},
					ServerName: "orders-pxc.team-a.svc",
				}
			},
			want: map[string]any{
				"port": "metrics", "path": "/metrics", "scheme": "https",
				"tlsConfig": map[string]any{
					"ca":         map[string]any{"secret": map[string]any{"name": "orders-ca", "key": "ca.crt"}},
					"serverName": "orders-pxc.team-a.svc",
				},
				"relabelConfigs": engineRelabelConfigs(),
			},
		},
		{
			name: "tls without verification",
			endpoint: func(ep *corev1alpha1.MetricsEndpoint) {
				ep.Scheme = "https"
				ep.TLS = &corev1alpha1.MetricsEndpointTLS{InsecureSkipVerify: true}
			},
			want: map[string]any{
				"port": "metrics", "path": "/metrics", "scheme": "https",
				"tlsConfig":      map[string]any{"insecureSkipVerify": true},
				"relabelConfigs": engineRelabelConfigs(),
			},
		},
		{
			name: "basic auth from the credential profile's secret",
			endpoint: func(ep *corev1alpha1.MetricsEndpoint) {
				ep.Auth = &corev1alpha1.MetricsEndpointAuth{CredentialProfile: "metrics"}
			},
			credentials: []corev1alpha1.MonitoringCredential{
				{Profile: "queryAnalytics", SecretRef: common.SecretRef{Name: "orders-monitoring-qan"}},
				{Profile: "metrics", SecretRef: common.SecretRef{Name: "orders-monitoring-metrics"}},
			},
			want: map[string]any{
				"port": "metrics", "path": "/metrics", "scheme": "http",
				"basicAuth": map[string]any{
					"username": map[string]any{"name": "orders-monitoring-metrics", "key": "username"},
					"password": map[string]any{"name": "orders-monitoring-metrics", "key": "password"},
				},
				"relabelConfigs": engineRelabelConfigs(),
			},
		},
		{
			name:       "interval from the destination",
			parameters: `{"interval":"30s"}`,
			want: map[string]any{
				"port": "metrics", "path": "/metrics", "scheme": "http", "interval": "30s",
				"relabelConfigs": engineRelabelConfigs(),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ep := engineEndpoint(intstr.FromString("metrics"))
			if tt.endpoint != nil {
				tt.endpoint(&ep)
			}
			sources := &corev1alpha1.MonitoringSources{Identity: identity(), Credentials: tt.credentials, Metrics: []corev1alpha1.MetricsEndpoint{ep}}
			c := newFakeClient(t, newBinding(), newInstance(sources), newDestination(tt.parameters))

			reconcileBinding(t, c)

			scrape, ok := vmPodScrapes(t, c)[testBinding+"-engine"]
			require.True(t, ok, "VMPodScrape of the engine component")
			endpoints, found, err := unstructured.NestedSlice(scrape.Object, "spec", "podMetricsEndpoints")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, []any{tt.want}, endpoints)
			cond := configured(t, c)
			require.NotNil(t, cond)
			assert.Equal(t, string(monitoringv1alpha1.ReasonConfigured), cond.Reason)
		})
	}
}

func TestReconcileRendersOneVMPodScrapePerComponent(t *testing.T) {
	t.Parallel()
	proxySelector := map[string]string{"core.openeverest.io/instance": testInstance, "core.openeverest.io/component": "proxy"}
	configSelector := map[string]string{"core.openeverest.io/instance": testInstance, "core.openeverest.io/component": "configServer"}
	sources := &corev1alpha1.MonitoringSources{Identity: identity(), Metrics: []corev1alpha1.MetricsEndpoint{
		engineEndpoint(intstr.FromString("metrics")),
		{Component: "proxy", Kind: "haproxy", PodSelector: proxySelector, Port: intstr.FromInt32(8404), Path: "/metrics"},
		{Component: "configServer", Kind: "mongodb", PodSelector: configSelector, Port: intstr.FromString("metrics")},
		{Component: "proxy", Kind: "haproxy", PodSelector: proxySelector, Port: intstr.FromString("stats"), Path: "/stats"},
	}}
	c := newFakeClient(t, newBinding(), newInstance(sources), newDestination(`{"vmPodScrapeLabels":{"vmagent":"main"}}`))

	reconcileBinding(t, c)

	scrapes := vmPodScrapes(t, c)
	names := []string{testBinding + "-configserver", testBinding + "-engine", testBinding + "-proxy"}
	assert.ElementsMatch(t, names, keys(scrapes))

	proxy := scrapes[testBinding+"-proxy"]
	require.NotNil(t, proxy)
	assert.Equal(t, map[string]string{
		"vmagent":                               "main",
		monitoringv1alpha1.BindingInstanceLabel: testInstance,
		monitoringv1alpha1.BindingClassLabel:    testClass,
	}, proxy.GetLabels())
	assert.Equal(t, []metav1.OwnerReference{{
		APIVersion: "monitoring.openeverest.io/v1alpha1", Kind: "MonitoringBinding", Name: testBinding, UID: testBindingUID,
		Controller: new(true),
	}}, proxy.GetOwnerReferences())
	selector, _, err := unstructured.NestedStringMap(proxy.Object, "spec", "selector", "matchLabels")
	require.NoError(t, err)
	assert.Equal(t, proxySelector, selector)
	namespaces, _, err := unstructured.NestedStringSlice(proxy.Object, "spec", "namespaceSelector", "matchNames")
	require.NoError(t, err)
	assert.Equal(t, []string{testNamespace}, namespaces)
	endpoints, _, err := unstructured.NestedSlice(proxy.Object, "spec", "podMetricsEndpoints")
	require.NoError(t, err)
	require.Len(t, endpoints, 2)
	first, ok := endpoints[0].(map[string]any)
	require.True(t, ok)
	second, ok := endpoints[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, int64(8404), first["portNumber"])
	assert.Equal(t, "stats", second["port"])

	b := &monitoringv1alpha1.MonitoringBinding{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: testBinding}, b))
	cond := meta.FindStatusCondition(b.Status.Conditions, monitoringv1alpha1.MonitoringBindingConditionConfigured)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "VMPodScrapes "+strings.Join(names, ", "), cond.Message)
	assert.True(t, meta.IsStatusConditionTrue(b.Status.Conditions, monitoringv1alpha1.MonitoringBindingConditionAccepted), "core's Accepted condition survives")

	dst := &monitoringv1alpha1.MonitoringDestination{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: testDestination}, dst))
	ready := meta.FindStatusCondition(dst.Status.Conditions, monitoringv1alpha1.MonitoringDestinationConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, string(monitoringv1alpha1.ReasonReady), ready.Reason)
	assert.Equal(t, "VMPodScrapes are labelled vmagent=main", ready.Message)
}

func TestReconcileRenderFailed(t *testing.T) {
	t.Parallel()

	otherSelector := engineEndpoint(intstr.FromInt32(9104))
	otherSelector.PodSelector = map[string]string{"core.openeverest.io/instance": testInstance}
	withAuth := engineEndpoint(intstr.FromString("metrics"))
	withAuth.Auth = &corev1alpha1.MetricsEndpointAuth{CredentialProfile: "metrics"}
	tests := []struct {
		name        string
		parameters  string
		metrics     []corev1alpha1.MetricsEndpoint
		wantMessage string
	}{
		{
			name:        "destination parameters of the wrong type",
			parameters:  `{"vmPodScrapeLabels":"vmagent=main"}`,
			metrics:     []corev1alpha1.MetricsEndpoint{engineEndpoint(intstr.FromString("metrics"))},
			wantMessage: "destination parameters",
		},
		{
			name:        "credential profile the Instance does not publish",
			metrics:     []corev1alpha1.MetricsEndpoint{withAuth},
			wantMessage: `credential profile "metrics"`,
		},
		{
			name:        "endpoints of one component with different pod selectors",
			metrics:     []corev1alpha1.MetricsEndpoint{engineEndpoint(intstr.FromString("metrics")), otherSelector},
			wantMessage: "component engine publishes metrics endpoints with different pod selectors",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sources := &corev1alpha1.MonitoringSources{Identity: identity(), Metrics: tt.metrics}
			c := newFakeClient(t, newBinding(), newInstance(sources), newDestination(tt.parameters))

			reconcileBinding(t, c)

			cond := configured(t, c)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, string(monitoringv1alpha1.ReasonRenderFailed), cond.Reason)
			assert.Contains(t, cond.Message, tt.wantMessage)
			assert.Empty(t, vmPodScrapes(t, c))
		})
	}
}

func TestReconcileWithoutVMOperatorCRDs(t *testing.T) {
	t.Parallel()
	// The client's REST mapper answers this way while the VMPodScrape CRD is not installed.
	noVMOperator := interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "operator.victoriametrics.com", Kind: vmPodScrapeKind}, SearchedVersions: []string{"v1beta1"}}
		},
	}
	c := newFakeClientBuilder(t, newBinding(), newInstance(engineSources()), newDestination("")).WithInterceptorFuncs(noVMOperator).Build()

	res := reconcileBinding(t, c)

	assert.Equal(t, time.Minute, res.RequeueAfter)
	cond := configured(t, c)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, string(monitoringv1alpha1.ReasonApplyFailed), cond.Reason)
	assert.Contains(t, cond.Message, "VMPodScrape "+testBinding+"-engine")
	assert.Contains(t, cond.Message, "the VictoriaMetrics Operator CRDs are not installed")
}

func TestReconcileWithoutServingEndpoints(t *testing.T) {
	t.Parallel()
	c := newFakeClient(t, newBinding(), newInstance(nil), newDestination(""))

	reconcileBinding(t, c)

	cond := configured(t, c)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, string(monitoringv1alpha1.ReasonEndpointNotServing), cond.Reason)
	assert.Empty(t, vmPodScrapes(t, c))
}

func ownedScrape(name, ownerUID string) *unstructured.Unstructured {
	s := &unstructured.Unstructured{}
	s.SetAPIVersion(vmPodScrapeAPIVersion)
	s.SetKind(vmPodScrapeKind)
	s.SetName(name)
	s.SetNamespace(testNamespace)
	s.SetLabels(map[string]string{monitoringv1alpha1.BindingInstanceLabel: testInstance})
	s.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "monitoring.openeverest.io/v1alpha1", Kind: "MonitoringBinding", Name: name, UID: types.UID(ownerUID), Controller: new(true),
	}})
	return s
}

func TestReconcilePrunesVMPodScrapesOfVanishedComponents(t *testing.T) {
	t.Parallel()
	c := newFakeClient(t, newBinding(), newInstance(engineSources()), newDestination(""),
		ownedScrape(testBinding+"-proxy", string(testBindingUID)),
		ownedScrape("orders-prometheus-9f8e7d6c-proxy", "other-binding-uid"),
	)

	reconcileBinding(t, c)

	assert.ElementsMatch(t, []string{testBinding + "-engine", "orders-prometheus-9f8e7d6c-proxy"}, keys(vmPodScrapes(t, c)))
}

func TestReconcileNotAccepted(t *testing.T) {
	t.Parallel()

	wasConfigured := metav1.Condition{
		Type: monitoringv1alpha1.MonitoringBindingConditionConfigured, Status: metav1.ConditionTrue,
		Reason: string(monitoringv1alpha1.ReasonConfigured), ObservedGeneration: 1, LastTransitionTime: metav1.Now(),
	}
	tests := []struct {
		name        string
		conditions  []metav1.Condition
		generation  int64
		wantReason  monitoringv1alpha1.ConfiguredReason
		wantStatus  metav1.ConditionStatus
		wantMessage string
	}{
		{
			name:        "core has not decided",
			conditions:  []metav1.Condition{accepted(metav1.ConditionUnknown, "Pending")},
			wantReason:  monitoringv1alpha1.ReasonPending,
			wantStatus:  metav1.ConditionUnknown,
			wantMessage: "waiting for the binding to be accepted",
		},
		{
			name:        "rejected before anything was rendered",
			conditions:  []metav1.Condition{accepted(metav1.ConditionFalse, monitoringv1alpha1.ReasonDestinationNotFound)},
			wantReason:  monitoringv1alpha1.ReasonNotAccepted,
			wantStatus:  metav1.ConditionFalse,
			wantMessage: "the binding is not accepted: DestinationNotFound",
		},
		{
			name:        "rejected after the scrapes were rendered",
			conditions:  []metav1.Condition{accepted(metav1.ConditionFalse, monitoringv1alpha1.ReasonDestinationNotFound), wasConfigured},
			wantReason:  monitoringv1alpha1.ReasonRetained,
			wantStatus:  metav1.ConditionUnknown,
			wantMessage: "keeping the current VMPodScrapes while the binding is not accepted: DestinationNotFound",
		},
		{
			name:        "new generation not accepted yet",
			conditions:  []metav1.Condition{accepted(metav1.ConditionTrue, monitoringv1alpha1.ReasonAccepted), wasConfigured},
			generation:  2,
			wantReason:  monitoringv1alpha1.ReasonRetained,
			wantStatus:  metav1.ConditionUnknown,
			wantMessage: "keeping the current VMPodScrapes while the binding is not accepted: Pending",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := newBinding(tt.conditions...)
			if tt.generation != 0 {
				b.Generation = tt.generation
			}
			live := ownedScrape(testBinding+"-engine", string(testBindingUID))
			c := newFakeClient(t, b, newInstance(engineSources()), newDestination(""), live)

			reconcileBinding(t, c)

			cond := configured(t, c)
			require.NotNil(t, cond)
			assert.Equal(t, tt.wantStatus, cond.Status)
			assert.Equal(t, string(tt.wantReason), cond.Reason)
			assert.Equal(t, tt.wantMessage, cond.Message)
			assert.Equal(t, []string{testBinding + "-engine"}, keys(vmPodScrapes(t, c)), "live VMPodScrapes are kept")
		})
	}
}

func TestReconcileIgnoresOtherBindings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		binding func(b *monitoringv1alpha1.MonitoringBinding)
	}{
		{
			name: "other class",
			binding: func(b *monitoringv1alpha1.MonitoringBinding) {
				b.Labels[monitoringv1alpha1.BindingClassLabel] = "prometheus"
			},
		},
		{
			name: "ProviderManaged",
			binding: func(b *monitoringv1alpha1.MonitoringBinding) {
				b.Spec.ExecutionMode = corev1alpha1.MonitoringExecutionModeProviderManaged
			},
		},
		{
			name:    "mode not resolved yet",
			binding: func(b *monitoringv1alpha1.MonitoringBinding) { b.Spec.ExecutionMode = "" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := newBinding()
			tt.binding(b)
			c := newFakeClient(t, b, newInstance(engineSources()), newDestination(""))

			reconcileBinding(t, c)

			assert.Nil(t, configured(t, c))
			assert.Empty(t, vmPodScrapes(t, c))
		})
	}
}

func TestClaimClass(t *testing.T) {
	t.Parallel()

	errRejected := errors.New("status apply rejected")
	tests := []struct {
		name           string
		controllerName string
		applyErr       error
		want           *metav1.Condition
	}{
		{
			name:           "own class",
			controllerName: ControllerName,
			want: &metav1.Condition{
				Type: monitoringv1alpha1.MonitoringClassConditionAccepted, Status: metav1.ConditionTrue,
				Reason: monitoringv1alpha1.ReasonAccepted, Message: "claimed by " + ControllerName, ObservedGeneration: 1,
			},
		},
		{
			name:           "class of another controller",
			controllerName: "openeverest.io/monitoring-prometheus",
		},
		{
			name:           "rejected claim is returned for a retry",
			controllerName: ControllerName,
			applyErr:       errRejected,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			class := &monitoringv1alpha1.MonitoringClass{
				ObjectMeta: metav1.ObjectMeta{Name: testClass, Generation: 1},
				Spec:       monitoringv1alpha1.MonitoringClassSpec{ControllerName: tt.controllerName},
			}
			builder := newFakeClientBuilder(t, class)
			if tt.applyErr != nil {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					SubResourceApply: func(context.Context, client.Client, string, runtime.ApplyConfiguration, ...client.SubResourceApplyOption) error {
						return tt.applyErr
					},
				})
			}
			c := builder.Build()

			_, err := NewReconciler(c, testClass).claim(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: testClass}})

			require.ErrorIs(t, err, tt.applyErr)
			got := &monitoringv1alpha1.MonitoringClass{}
			require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: testClass}, got))
			cond := meta.FindStatusCondition(got.Status.Conditions, monitoringv1alpha1.MonitoringClassConditionAccepted)
			if tt.want == nil {
				assert.Nil(t, cond)
				return
			}
			require.NotNil(t, cond)
			cond.LastTransitionTime = metav1.Time{}
			assert.Equal(t, *tt.want, *cond)
		})
	}
}

func TestBindingsForInstanceAndDestination(t *testing.T) {
	t.Parallel()

	binding := func(name, instance, destination, class string, mode corev1alpha1.MonitoringExecutionMode) *monitoringv1alpha1.MonitoringBinding {
		return &monitoringv1alpha1.MonitoringBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: monitoring.BindingLabels(instance, name, destination, class)},
			Spec:       monitoringv1alpha1.MonitoringBindingSpec{ExecutionMode: mode},
		}
	}
	c := newFakeClient(t,
		binding("orders-vm", testInstance, testDestination, testClass, corev1alpha1.MonitoringExecutionModeExtensionManaged),
		binding("orders-prom", testInstance, "prom", "prometheus", corev1alpha1.MonitoringExecutionModeExtensionManaged),
		binding("orders-pmm", testInstance, testDestination, testClass, corev1alpha1.MonitoringExecutionModeProviderManaged),
		binding("billing-vm", "billing", testDestination, testClass, corev1alpha1.MonitoringExecutionModeExtensionManaged),
	)
	r := NewReconciler(c, testClass)

	request := func(name string) reconcile.Request {
		return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name}}
	}
	assert.Equal(t, []reconcile.Request{request("orders-vm")}, r.bindingsForInstance(t.Context(), newInstance(nil)))
	assert.ElementsMatch(t, []reconcile.Request{request("billing-vm"), request("orders-vm")}, r.bindingsForDestination(t.Context(), newDestination("")))
}

func keys(m map[string]*unstructured.Unstructured) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

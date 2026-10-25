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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	"sigs.k8s.io/yaml"

	common "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

// crdModels holds the OpenAPI models of the monitoring CRDs shipped with the
// pinned core module, parsed once for all tests.
var crdModels = sync.OnceValues(loadCRDModels) //nolint:gochecknoglobals // parsed once per test binary

func loadCRDModels() (map[string]*spec.Schema, error) {
	out, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}", "github.com/openeverest/openeverest/v2").Output()
	if err != nil {
		return nil, err
	}
	crds := os.DirFS(filepath.Join(strings.TrimSpace(string(out)), "config", "crd", "bases"))
	models := map[string]*spec.Schema{}
	for _, plural := range []string{"monitoringclasses", "monitoringdestinations", "monitoringbindings"} {
		raw, err := fs.ReadFile(crds, "monitoring.openeverest.io_"+plural+".yaml")
		if err != nil {
			return nil, err
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(raw, crd); err != nil {
			return nil, err
		}
		for _, v := range crd.Spec.Versions {
			js, err := json.Marshal(v.Schema.OpenAPIV3Schema)
			if err != nil {
				return nil, err
			}
			s := &spec.Schema{}
			if err := json.Unmarshal(js, s); err != nil {
				return nil, err
			}
			s.AddExtension("x-kubernetes-group-version-kind", []any{
				map[string]any{"group": crd.Spec.Group, "version": v.Name, "kind": crd.Spec.Names.Kind},
			})
			models[crd.Spec.Group+"."+v.Name+"."+crd.Spec.Names.Kind] = s
		}
	}
	return models, nil
}

// crdTypeConverter makes the fake client's server-side apply follow the CRD
// schemas (listType=map conditions) instead of treating every list as atomic.
func crdTypeConverter(t *testing.T) managedfields.TypeConverter { //nolint:ireturn // the fake client takes the interface
	t.Helper()
	models, err := crdModels()
	require.NoError(t, err)
	tc, err := managedfields.NewTypeConverter(models, false)
	require.NoError(t, err)
	return tc
}

func newFakeClient(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) client.Client { //nolint:ireturn // the fake is used through the client interface
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(s))
	require.NoError(t, monitoringv1alpha1.AddToScheme(s))
	b := fake.NewClientBuilder().
		WithScheme(s).
		WithTypeConverters(crdTypeConverter(t), managedfields.NewDeducedTypeConverter()).
		WithObjects(objs...).
		WithStatusSubresource(
			&corev1alpha1.Instance{},
			&monitoringv1alpha1.MonitoringClass{},
			&monitoringv1alpha1.MonitoringDestination{},
			&monitoringv1alpha1.MonitoringBinding{},
		)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

func testClass(controllerName string) *monitoringv1alpha1.MonitoringClass {
	return &monitoringv1alpha1.MonitoringClass{
		ObjectMeta: metav1.ObjectMeta{Name: "victoriametrics", Generation: 1},
		Spec: monitoringv1alpha1.MonitoringClassSpec{
			ControllerName:   controllerName,
			ExtensionManaged: &monitoringv1alpha1.ExtensionManagedFulfilment{},
		},
	}
}

func testDestination(parameters string) *monitoringv1alpha1.MonitoringDestination {
	dst := &monitoringv1alpha1.MonitoringDestination{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-prod", Namespace: testNamespace, Generation: 1},
		Spec:       monitoringv1alpha1.MonitoringDestinationSpec{ClassRef: common.ObjectRef{Name: "victoriametrics"}},
	}
	if parameters != "" {
		dst.Spec.Parameters = &runtime.RawExtension{Raw: []byte(parameters)}
	}
	return dst
}

func testInstance(metrics ...corev1alpha1.MetricsEndpoint) *corev1alpha1.Instance {
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace},
		Spec:       corev1alpha1.InstanceSpec{ProviderRef: common.ObjectRef{Name: "percona-xtradb-cluster"}},
		Status: corev1alpha1.InstanceStatus{Monitoring: &corev1alpha1.InstanceMonitoringStatus{
			Sources: &corev1alpha1.MonitoringSources{
				Identity:    testIdentity(),
				Credentials: []corev1alpha1.MonitoringCredential{{Profile: "metrics", SecretRef: common.SecretRef{Name: "orders-monitoring-metrics"}}},
				Metrics:     metrics,
			},
		}},
	}
}

func engineEndpoint() corev1alpha1.MetricsEndpoint {
	return corev1alpha1.MetricsEndpoint{
		Component: "engine", Kind: "mysql", PodSelector: engineSelector(), Port: intstr.FromString("metrics"),
		Auth: &corev1alpha1.MetricsEndpointAuth{CredentialProfile: "metrics"},
	}
}

func proxyEndpoint() corev1alpha1.MetricsEndpoint {
	return corev1alpha1.MetricsEndpoint{
		Component: "proxy", Kind: "haproxy", Port: intstr.FromInt32(8404),
		PodSelector: map[string]string{monitoringv1alpha1.BindingInstanceLabel: "orders", "app.kubernetes.io/component": "haproxy"},
	}
}

func acceptedCondition(generation int64) metav1.Condition {
	return metav1.Condition{
		Type: monitoringv1alpha1.MonitoringBindingConditionAccepted, Status: metav1.ConditionTrue, Reason: monitoringv1alpha1.ReasonAccepted,
		ObservedGeneration: generation, LastTransitionTime: metav1.Now(),
	}
}

func acceptedBinding() *monitoringv1alpha1.MonitoringBinding {
	b := testBinding()
	b.Status.Conditions = []metav1.Condition{acceptedCondition(1)}
	return b
}

func reconcileBinding(t *testing.T, c client.Client) ctrl.Result {
	t.Helper()
	r := &bindingReconciler{client: c, cc: monitoring.ClassController{Client: c, ControllerName: ControllerName}, className: "victoriametrics"}
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: testBindingName}})
	require.NoError(t, err)
	return res
}

func bindingCondition(t *testing.T, c client.Client, condType string) *metav1.Condition {
	t.Helper()
	b := &monitoringv1alpha1.MonitoringBinding{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: testBindingName}, b))
	return meta.FindStatusCondition(b.Status.Conditions, condType)
}

// liveVMPodScrapes returns the VMPodScrapes stored in the fake API server.
func liveVMPodScrapes(t *testing.T, c client.Client) map[string]vmPodScrape {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "operator.victoriametrics.com", Version: "v1beta1", Kind: "VMPodScrapeList"})
	require.NoError(t, c.List(t.Context(), list, client.InNamespace(testNamespace)))
	out := map[string]vmPodScrape{}
	for _, u := range list.Items {
		data, err := u.MarshalJSON()
		require.NoError(t, err)
		s := vmPodScrape{}
		require.NoError(t, json.Unmarshal(data, &s))
		out[s.Name] = s
	}
	return out
}

// ownedVMPodScrape is a VMPodScrape controlled by the binding with ownerUID.
func ownedVMPodScrape(name string, ownerUID types.UID) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("operator.victoriametrics.com/v1beta1")
	u.SetKind("VMPodScrape")
	u.SetNamespace(testNamespace)
	u.SetName(name)
	u.SetLabels(map[string]string{monitoringv1alpha1.BindingInstanceLabel: "orders", monitoringv1alpha1.BindingClassLabel: "victoriametrics"})
	u.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "monitoring.openeverest.io/v1alpha1", Kind: "MonitoringBinding", Name: name, UID: ownerUID, Controller: new(true),
	}})
	return u
}

func TestReconcileAppliesVMPodScrapesAndReportsConfigured(t *testing.T) {
	t.Parallel()
	c := newFakeClient(t, nil,
		testClass(ControllerName),
		testDestination(`{"vmPodScrapeLabels":{"vmagent":"main"},"interval":"30s"}`),
		testInstance(engineEndpoint(), proxyEndpoint()),
		acceptedBinding(),
	)

	res := reconcileBinding(t, c)
	assert.Zero(t, res)

	live := liveVMPodScrapes(t, c)
	require.Len(t, live, 2)
	engine := live[testBindingName+"-engine"]
	assert.Equal(t, "main", engine.Labels["vmagent"])
	require.Len(t, engine.OwnerReferences, 1)
	assert.Equal(t, testBindingUID, engine.OwnerReferences[0].UID)
	require.Len(t, engine.Spec.PodMetricsEndpoints, 1)
	assert.Equal(t, "30s", engine.Spec.PodMetricsEndpoints[0].Interval)
	require.NotNil(t, engine.Spec.PodMetricsEndpoints[0].BasicAuth)
	assert.Equal(t, "orders-monitoring-metrics", engine.Spec.PodMetricsEndpoints[0].BasicAuth.Password.Name)
	proxy := live[testBindingName+"-proxy"]
	require.Len(t, proxy.Spec.PodMetricsEndpoints, 1)
	assert.Equal(t, int32(8404), proxy.Spec.PodMetricsEndpoints[0].PortNumber)

	configured := bindingCondition(t, c, monitoringv1alpha1.MonitoringBindingConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, metav1.ConditionTrue, configured.Status)
	assert.Equal(t, string(monitoringv1alpha1.ReasonConfigured), configured.Reason)
	assert.Equal(t, "VMPodScrapes "+testBindingName+"-engine, "+testBindingName+"-proxy", configured.Message)
	accepted := bindingCondition(t, c, monitoringv1alpha1.MonitoringBindingConditionAccepted)
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status, "core's Accepted condition survives the controller's apply")

	dst := &monitoringv1alpha1.MonitoringDestination{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "vm-prod"}, dst))
	ready := meta.FindStatusCondition(dst.Status.Conditions, monitoringv1alpha1.MonitoringDestinationConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, string(monitoringv1alpha1.ReasonReady), ready.Reason)
	assert.Equal(t, "VMPodScrapes are labelled vmagent=main", ready.Message)
}

func TestReconcileReportsWhyNothingIsConfigured(t *testing.T) {
	t.Parallel()

	noVMOperatorCRDs := &interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "operator.victoriametrics.com", Kind: "VMPodScrape"}, SearchedVersions: []string{"v1beta1"}}
		},
	}

	tests := []struct {
		name        string
		destination *monitoringv1alpha1.MonitoringDestination
		instance    *corev1alpha1.Instance
		funcs       *interceptor.Funcs
		wantReason  monitoringv1alpha1.ConfiguredReason
		wantMessage string
		wantRequeue bool
	}{
		{
			name:        "destination parameters that cannot be decoded",
			destination: testDestination(`{"vmPodScrapeLabels":"vmagent=main"}`),
			instance:    testInstance(engineEndpoint()),
			wantReason:  monitoringv1alpha1.ReasonRenderFailed,
			wantMessage: "MonitoringDestination vm-prod: decode parameters",
		},
		{
			name:        "destination gone",
			instance:    testInstance(engineEndpoint()),
			wantReason:  monitoringv1alpha1.ReasonRenderFailed,
			wantMessage: `get MonitoringDestination "vm-prod"`,
		},
		{
			name:        "Instance publishes no endpoints yet",
			destination: testDestination(""),
			instance:    testInstance(),
			wantReason:  monitoringv1alpha1.ReasonEndpointNotServing,
			wantMessage: "Instance orders publishes no metrics endpoints yet",
		},
		{
			name:        "endpoint naming an unpublished credential profile",
			destination: testDestination(""),
			instance: testInstance(corev1alpha1.MetricsEndpoint{
				Component: "engine", Kind: "mysql", PodSelector: engineSelector(), Port: intstr.FromString("metrics"),
				Auth: &corev1alpha1.MetricsEndpointAuth{CredentialProfile: qanProfile},
			}),
			wantReason:  monitoringv1alpha1.ReasonRenderFailed,
			wantMessage: `credential profile "queryAnalytics" is not published by the Instance`,
		},
		{
			name:        "VictoriaMetrics Operator CRDs not installed",
			destination: testDestination(""),
			instance:    testInstance(engineEndpoint()),
			funcs:       noVMOperatorCRDs,
			wantReason:  monitoringv1alpha1.ReasonApplyFailed,
			wantMessage: `the VictoriaMetrics Operator CRDs are not installed: apply VMPodScrape ` + testBindingName + `-engine: no matches for kind "VMPodScrape"`,
			wantRequeue: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			objs := []client.Object{testClass(ControllerName), acceptedBinding(), tt.instance}
			if tt.destination != nil {
				objs = append(objs, tt.destination)
			}
			c := newFakeClient(t, tt.funcs, objs...)

			res := reconcileBinding(t, c)

			configured := bindingCondition(t, c, monitoringv1alpha1.MonitoringBindingConditionConfigured)
			require.NotNil(t, configured)
			assert.Equal(t, metav1.ConditionFalse, configured.Status)
			assert.Equal(t, string(tt.wantReason), configured.Reason)
			assert.Contains(t, configured.Message, tt.wantMessage)
			if tt.wantRequeue {
				assert.Equal(t, applyRetryInterval, res.RequeueAfter)
			} else {
				assert.Zero(t, res)
			}
			assert.Empty(t, liveVMPodScrapes(t, c))
		})
	}
}

func TestReconcileNotAccepted(t *testing.T) {
	t.Parallel()

	rejected := metav1.Condition{
		Type: monitoringv1alpha1.MonitoringBindingConditionAccepted, Status: metav1.ConditionFalse, ObservedGeneration: 1,
		Reason: monitoringv1alpha1.ReasonDestinationNotFound, LastTransitionTime: metav1.Now(),
	}
	wasConfigured := metav1.Condition{
		Type: monitoringv1alpha1.MonitoringBindingConditionConfigured, Status: metav1.ConditionTrue, ObservedGeneration: 1,
		Reason: string(monitoringv1alpha1.ReasonConfigured), LastTransitionTime: metav1.Now(),
	}
	tests := []struct {
		name        string
		conditions  []metav1.Condition
		generation  int64
		wantStatus  metav1.ConditionStatus
		wantReason  monitoringv1alpha1.ConfiguredReason
		wantMessage string
	}{
		{
			name:        "core has not decided",
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  monitoringv1alpha1.ReasonPending,
			wantMessage: "waiting for the binding to be accepted",
		},
		{
			name:        "rejected before anything was rendered",
			conditions:  []metav1.Condition{rejected},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  monitoringv1alpha1.ReasonNotAccepted,
			wantMessage: "the binding is not accepted: DestinationNotFound",
		},
		{
			name:        "rejected after the scrapes were rendered",
			conditions:  []metav1.Condition{rejected, wasConfigured},
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  monitoringv1alpha1.ReasonRetained,
			wantMessage: "keeping the current VMPodScrapes while the binding is not accepted: DestinationNotFound",
		},
		{
			name:        "new generation not accepted yet",
			conditions:  []metav1.Condition{acceptedCondition(1), wasConfigured},
			generation:  2,
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  monitoringv1alpha1.ReasonRetained,
			wantMessage: "keeping the current VMPodScrapes while the binding is not accepted: Pending",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := testBinding()
			b.Status.Conditions = tt.conditions
			if tt.generation != 0 {
				b.Generation = tt.generation
			}
			c := newFakeClient(t, nil, testClass(ControllerName), b, testDestination(""), testInstance(engineEndpoint()),
				ownedVMPodScrape(testBindingName+"-engine", testBindingUID))

			reconcileBinding(t, c)

			configured := bindingCondition(t, c, monitoringv1alpha1.MonitoringBindingConditionConfigured)
			require.NotNil(t, configured)
			assert.Equal(t, tt.wantStatus, configured.Status)
			assert.Equal(t, string(tt.wantReason), configured.Reason)
			assert.Equal(t, tt.wantMessage, configured.Message)
			assert.Contains(t, liveVMPodScrapes(t, c), testBindingName+"-engine", "live VMPodScrapes are kept")
		})
	}
}

func TestReconcileIgnoresBindingsItDoesNotServe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		binding func(b *monitoringv1alpha1.MonitoringBinding)
	}{
		{
			name: "binding of another class",
			binding: func(b *monitoringv1alpha1.MonitoringBinding) {
				b.Labels[monitoringv1alpha1.BindingClassLabel] = "prometheus"
			},
		},
		{
			name: "ProviderManaged binding",
			binding: func(b *monitoringv1alpha1.MonitoringBinding) {
				b.Spec.ExecutionMode = corev1alpha1.MonitoringExecutionModeProviderManaged
			},
		},
		{
			name:    "execution mode not resolved yet",
			binding: func(b *monitoringv1alpha1.MonitoringBinding) { b.Spec.ExecutionMode = "" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := acceptedBinding()
			tt.binding(b)
			c := newFakeClient(t, nil, testClass(ControllerName), testDestination(""), testInstance(engineEndpoint()), b)

			res := reconcileBinding(t, c)

			assert.Zero(t, res)
			assert.Nil(t, bindingCondition(t, c, monitoringv1alpha1.MonitoringBindingConditionConfigured))
			assert.Empty(t, liveVMPodScrapes(t, c))
		})
	}
}

func TestReconcilePrunesVMPodScrapesOfComponentsNoLongerPublished(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newFakeClient(t, nil,
		testClass(ControllerName),
		testDestination(""),
		testInstance(engineEndpoint(), proxyEndpoint()),
		acceptedBinding(),
		ownedVMPodScrape("orders-vm-0000000-proxy", "other"),
	)
	reconcileBinding(t, c)
	require.Len(t, liveVMPodScrapes(t, c), 3)

	in := &corev1alpha1.Instance{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "orders"}, in))
	in.Status.Monitoring.Sources.Metrics = []corev1alpha1.MetricsEndpoint{engineEndpoint()}
	require.NoError(t, c.Status().Update(ctx, in))
	reconcileBinding(t, c)

	live := liveVMPodScrapes(t, c)
	assert.Len(t, live, 2)
	assert.Contains(t, live, testBindingName+"-engine")
	assert.Contains(t, live, "orders-vm-0000000-proxy", "VMPodScrapes of other bindings are left alone")
	configured := bindingCondition(t, c, monitoringv1alpha1.MonitoringBindingConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, "VMPodScrapes "+testBindingName+"-engine", configured.Message)
}

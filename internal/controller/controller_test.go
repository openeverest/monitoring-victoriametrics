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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

func TestClassReconcilerClaimsOurClass(t *testing.T) {
	t.Parallel()

	otherClass := testClass()
	otherClass.Name = "victoriametrics-eu"
	errRejected := errors.New("status apply rejected")
	tests := []struct {
		name     string
		class    *monitoringv1alpha1.MonitoringClass
		applyErr error
		want     *metav1.Condition
	}{
		{
			name:  "class this controller serves",
			class: testClass(),
			want: &metav1.Condition{
				Status: metav1.ConditionTrue, Reason: monitoringv1alpha1.ReasonAccepted, Message: "claimed by " + FieldOwner, ObservedGeneration: 1,
			},
		},
		{
			name:  "another class",
			class: otherClass,
		},
		{
			name:     "rejected claim is returned for a retry",
			class:    testClass(),
			applyErr: errRejected,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			var funcs *interceptor.Funcs
			if tt.applyErr != nil {
				funcs = &interceptor.Funcs{
					SubResourceApply: func(context.Context, client.Client, string, runtime.ApplyConfiguration, ...client.SubResourceApplyOption) error {
						return tt.applyErr
					},
				}
			}
			c := newFakeClient(t, funcs, tt.class)
			r := &classReconciler{client: c, cc: monitoring.ClassController{Client: c, FieldOwner: FieldOwner, ClassName: "victoriametrics"}}

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: tt.class.Name}})
			require.ErrorIs(t, err, tt.applyErr)

			class := &monitoringv1alpha1.MonitoringClass{}
			require.NoError(t, c.Get(ctx, client.ObjectKey{Name: tt.class.Name}, class))
			got := meta.FindStatusCondition(class.Status.Conditions, monitoringv1alpha1.MonitoringClassConditionAccepted)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.want.Status, got.Status)
			assert.Equal(t, tt.want.Reason, got.Reason)
			assert.Equal(t, tt.want.Message, got.Message)
			assert.Equal(t, tt.want.ObservedGeneration, got.ObservedGeneration)
		})
	}
}

func TestDestinationReconcilerReportsReady(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		className  string
		parameters string
		wasReady   bool
		want       *metav1.Condition
	}{
		{
			name:       "labels to be selected by",
			className:  "victoriametrics",
			parameters: `{"vmPodScrapeLabels":{"vmagent":"main","team":"dba"}}`,
			want:       &metav1.Condition{Status: metav1.ConditionTrue, Reason: string(monitoringv1alpha1.ReasonReady), Message: "VMPodScrapes are labelled team=dba,vmagent=main"},
		},
		{
			name:      "no parameters",
			className: "victoriametrics",
			want:      &metav1.Condition{Status: metav1.ConditionTrue, Reason: string(monitoringv1alpha1.ReasonReady), Message: "VMPodScrapes are labelled {}"},
		},
		{
			name:       "parameters that cannot be rendered",
			className:  "victoriametrics",
			parameters: `{"interval":"often"}`,
			want: &metav1.Condition{
				Status: metav1.ConditionFalse, Reason: string(monitoringv1alpha1.ReasonReadyInvalidParameters),
				Message: `interval "often" is not a Prometheus duration such as 30s or 1m`,
			},
		},
		{
			name:       "parameters of a Ready destination broke",
			className:  "victoriametrics",
			parameters: `{"vmPodScrapeLabels":{"vmagent":"main"},"interval":"30 seconds"}`,
			wasReady:   true,
			want: &metav1.Condition{
				Status: metav1.ConditionFalse, Reason: string(monitoringv1alpha1.ReasonReadyInvalidParameters),
				Message: `interval "30 seconds" is not a Prometheus duration such as 30s or 1m`,
			},
		},
		{
			name:       "destination of another class",
			className:  "prometheus",
			parameters: `{"vmPodScrapeLabels":{"vmagent":"main"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dst := testDestination(tt.parameters)
			dst.Spec.ClassRef.Name = tt.className
			if tt.wasReady {
				dst.Status.Conditions = []metav1.Condition{{
					Type: monitoringv1alpha1.MonitoringDestinationConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1,
					Reason: string(monitoringv1alpha1.ReasonReady), Message: "VMPodScrapes are labelled vmagent=main", LastTransitionTime: metav1.Now(),
				}}
			}
			c := newFakeClient(t, nil, testClass(), dst)
			r := &destinationReconciler{client: c, cc: monitoring.ClassController{Client: c, FieldOwner: FieldOwner, ClassName: "victoriametrics"}}

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: "vm-prod"}})
			require.NoError(t, err)

			got := &monitoringv1alpha1.MonitoringDestination{}
			require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "vm-prod"}, got))
			ready := meta.FindStatusCondition(got.Status.Conditions, monitoringv1alpha1.MonitoringDestinationConditionReady)
			if tt.want == nil {
				assert.Nil(t, ready)
				return
			}
			require.NotNil(t, ready)
			assert.Equal(t, tt.want.Status, ready.Status)
			assert.Equal(t, tt.want.Reason, ready.Reason)
			assert.Equal(t, tt.want.Message, ready.Message)
			assert.Equal(t, int64(1), ready.ObservedGeneration)
		})
	}
}

func TestSourcesChanged(t *testing.T) {
	t.Parallel()

	withPhase := func(in *corev1alpha1.Instance, phase string) *corev1alpha1.Instance {
		in.Status.Phase = corev1alpha1.InstancePhase(phase)
		return in
	}
	tests := []struct {
		name     string
		old, new *corev1alpha1.Instance
		want     bool
	}{
		{
			name: "unrelated status change",
			old:  withPhase(testInstance(engineEndpoint()), "Creating"),
			new:  withPhase(testInstance(engineEndpoint()), "Running"),
			want: false,
		},
		{
			name: "endpoint published",
			old:  testInstance(),
			new:  testInstance(engineEndpoint()),
			want: true,
		},
		{
			name: "endpoint withdrawn",
			old:  testInstance(engineEndpoint(), proxyEndpoint()),
			new:  testInstance(engineEndpoint()),
			want: true,
		},
		{
			name: "sources first published",
			old:  &corev1alpha1.Instance{},
			new:  testInstance(),
			want: true,
		},
	}
	p := sourcesChanged()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, p.Update(event.UpdateEvent{ObjectOld: tt.old, ObjectNew: tt.new}))
		})
	}
	assert.False(t, p.Create(event.CreateEvent{Object: testInstance(engineEndpoint())}), "bindings are enqueued by their own create events")
}

func TestBindingsForInstanceAndDestination(t *testing.T) {
	t.Parallel()

	binding := func(name, instance, class string, mode corev1alpha1.MonitoringExecutionMode, controller types.UID) *monitoringv1alpha1.MonitoringBinding {
		return &monitoringv1alpha1.MonitoringBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: testNamespace,
				Labels: monitoring.BindingLabels(instance, name, "vm-prod", class),
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "core.openeverest.io/v1alpha1", Kind: "Instance", Name: instance, UID: controller, Controller: new(true),
				}},
			},
			Spec: monitoringv1alpha1.MonitoringBindingSpec{ExecutionMode: mode},
		}
	}
	extension, provider := corev1alpha1.MonitoringExecutionModeExtensionManaged, corev1alpha1.MonitoringExecutionModeProviderManaged
	c := newFakeClient(t, nil,
		binding("orders-vm", "orders", "victoriametrics", extension, "orders-uid"),
		binding("orders-prom", "orders", "prometheus", extension, "orders-uid"),
		binding("orders-pmm", "orders", "victoriametrics", provider, "orders-uid"),
		binding("orders-old-vm", "orders", "victoriametrics", extension, "deleted-orders-uid"),
		binding("billing-vm", "billing", "victoriametrics", extension, "billing-uid"),
	)
	r := &bindingReconciler{client: c, cc: monitoring.ClassController{Client: c, FieldOwner: FieldOwner, ClassName: "victoriametrics"}, className: "victoriametrics"}
	in := testInstance()
	in.UID = "orders-uid"

	request := func(name string) reconcile.Request {
		return reconcile.Request{NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: name}}
	}
	assert.Equal(t, []reconcile.Request{request("orders-vm")}, r.bindingsForInstance(t.Context(), in),
		"only ExtensionManaged bindings of our class that the Instance controls")
	assert.ElementsMatch(t, []reconcile.Request{request("billing-vm"), request("orders-old-vm"), request("orders-vm")},
		r.bindingsForDestination(t.Context(), testDestination("")), "only ExtensionManaged bindings of our class")
}

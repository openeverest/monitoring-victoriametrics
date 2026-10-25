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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

func TestClassReconcilerClaimsOurClass(t *testing.T) {
	t.Parallel()

	errRejected := errors.New("status apply rejected")
	tests := []struct {
		name            string
		classController string
		applyErr        error
		want            *metav1.Condition
	}{
		{
			name:            "class naming this controller",
			classController: ControllerName,
			want: &metav1.Condition{
				Status: metav1.ConditionTrue, Reason: monitoringv1alpha1.ReasonAccepted, Message: "claimed by " + ControllerName, ObservedGeneration: 1,
			},
		},
		{
			name:            "class of another controller",
			classController: "openeverest.io/monitoring-prometheus",
		},
		{
			name:            "rejected claim is returned for a retry",
			classController: ControllerName,
			applyErr:        errRejected,
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
			c := newFakeClient(t, funcs, testClass(tt.classController))
			r := &classReconciler{client: c, cc: monitoring.ClassController{Client: c, ControllerName: ControllerName}}

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "victoriametrics"}})
			require.ErrorIs(t, err, tt.applyErr)

			class := &monitoringv1alpha1.MonitoringClass{}
			require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "victoriametrics"}, class))
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

func TestBindingsForInstanceAndDestination(t *testing.T) {
	t.Parallel()

	binding := func(name, instance, destination, class string, mode corev1alpha1.MonitoringExecutionMode) *monitoringv1alpha1.MonitoringBinding {
		return &monitoringv1alpha1.MonitoringBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: monitoring.BindingLabels(instance, name, destination, class)},
			Spec:       monitoringv1alpha1.MonitoringBindingSpec{ExecutionMode: mode},
		}
	}
	c := newFakeClient(t, nil,
		binding("orders-vm", "orders", "vm-prod", "victoriametrics", corev1alpha1.MonitoringExecutionModeExtensionManaged),
		binding("orders-prom", "orders", "prom", "prometheus", corev1alpha1.MonitoringExecutionModeExtensionManaged),
		binding("orders-pmm", "orders", "vm-prod", "victoriametrics", corev1alpha1.MonitoringExecutionModeProviderManaged),
		binding("billing-vm", "billing", "vm-prod", "victoriametrics", corev1alpha1.MonitoringExecutionModeExtensionManaged),
	)
	r := &bindingReconciler{client: c, cc: monitoring.ClassController{Client: c, ControllerName: ControllerName}, className: "victoriametrics"}

	request := func(name string) reconcile.Request {
		return reconcile.Request{NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: name}}
	}
	assert.Equal(t, []reconcile.Request{request("orders-vm")}, r.bindingsForInstance(t.Context(), testInstance()))
	assert.ElementsMatch(t, []reconcile.Request{request("billing-vm"), request("orders-vm")}, r.bindingsForDestination(t.Context(), testDestination("")))
}

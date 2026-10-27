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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	common "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
)

const (
	testNamespace   = "team-a"
	testBindingName = "orders-vm-1a2b3c4d"
	testBindingUID  = types.UID("6a0e4b8e-3f0c-4d1c-9a51-1f4b0c9e2d11")
	qanProfile      = "queryAnalytics"
)

func testBinding() *monitoringv1alpha1.MonitoringBinding {
	return &monitoringv1alpha1.MonitoringBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: testBindingName, Namespace: testNamespace, UID: testBindingUID, Generation: 1,
			Labels: map[string]string{
				monitoringv1alpha1.BindingInstanceLabel:    "orders",
				monitoringv1alpha1.BindingEntryLabel:       "vm",
				monitoringv1alpha1.BindingDestinationLabel: "vm-prod",
				monitoringv1alpha1.BindingClassLabel:       "victoriametrics",
			},
		},
		Spec: monitoringv1alpha1.MonitoringBindingSpec{
			InstanceRef:    common.ObjectRef{Name: "orders"},
			DestinationRef: common.MonitoringDestinationRef{Kind: "MonitoringDestination", Name: "vm-prod"},
			ClassRef:       &common.ObjectRef{Name: "victoriametrics"},
			ExecutionMode:  corev1alpha1.MonitoringExecutionModeExtensionManaged,
		},
	}
}

func testIdentity() corev1alpha1.MonitoringIdentity {
	return corev1alpha1.MonitoringIdentity{
		ClusterID: "c1",
		Labels: map[string]string{
			"openeverest_instance":  "orders",
			"openeverest_namespace": testNamespace,
			"openeverest_provider":  "percona-xtradb-cluster",
			"k8s_cluster_id":        "c1",
		},
	}
}

// identityRelabelConfigs is what every endpoint carries for testIdentity,
// in label name order.
func identityRelabelConfigs(component, kind string) []relabelConfig {
	return []relabelConfig{
		{TargetLabel: "k8s_cluster_id", Replacement: new("c1"), Action: "replace"},
		{TargetLabel: "openeverest_component", Replacement: new(component), Action: "replace"},
		{TargetLabel: "openeverest_instance", Replacement: new("orders"), Action: "replace"},
		{TargetLabel: "openeverest_kind", Replacement: new(kind), Action: "replace"},
		{TargetLabel: "openeverest_namespace", Replacement: new(testNamespace), Action: "replace"},
		{TargetLabel: "openeverest_provider", Replacement: new("percona-xtradb-cluster"), Action: "replace"},
	}
}

func engineSelector() map[string]string {
	return map[string]string{monitoringv1alpha1.BindingInstanceLabel: "orders", "app.kubernetes.io/component": "pxc"}
}

func TestVMPodScrapesForRendersOnePerComponent(t *testing.T) {
	t.Parallel()
	sources := &corev1alpha1.MonitoringSources{
		Identity: testIdentity(),
		Metrics: []corev1alpha1.MetricsEndpoint{
			{Component: "engine", Kind: "mysql", PodSelector: engineSelector(), Port: intstr.FromString("metrics")},
			{Component: "configServer", Kind: "mongodb", PodSelector: map[string]string{monitoringv1alpha1.BindingInstanceLabel: "orders", "app": "cfg"}, Port: intstr.FromInt32(9216)},
			{Component: "engine", Kind: "mysql", PodSelector: engineSelector(), Port: intstr.FromString("probe"), Path: "/probe"},
		},
	}
	params := destinationParameters{VMPodScrapeLabels: map[string]string{
		"vmagent":                               "main",
		monitoringv1alpha1.BindingInstanceLabel: "spoofed",
		monitoringv1alpha1.BindingClassLabel:    "spoofed",
	}}

	got, err := vmPodScrapesFor(testBinding(), "victoriametrics", sources, params)
	require.NoError(t, err)
	require.Len(t, got, 2)

	engine, configServer := got[0], got[1]
	assert.Equal(t, testBindingName+"-engine", engine.Name)
	assert.Equal(t, testBindingName+"-configserver", configServer.Name)
	assert.Equal(t, metav1.TypeMeta{APIVersion: "operator.victoriametrics.com/v1beta1", Kind: "VMPodScrape"}, engine.TypeMeta)
	assert.Equal(t, testNamespace, engine.Namespace)
	assert.Equal(t, map[string]string{
		"vmagent":                               "main",
		monitoringv1alpha1.BindingInstanceLabel: "orders",
		monitoringv1alpha1.BindingClassLabel:    "victoriametrics",
	}, engine.Labels, "destination labels cannot override the instance and class labels")
	assert.Equal(t, []metav1.OwnerReference{{
		APIVersion: "monitoring.openeverest.io/v1alpha1",
		Kind:       "MonitoringBinding",
		Name:       testBindingName,
		UID:        testBindingUID,
		Controller: new(true),
	}}, engine.OwnerReferences)
	assert.Equal(t, metav1.LabelSelector{MatchLabels: engineSelector()}, engine.Spec.Selector)
	assert.Equal(t, namespaceSelector{MatchNames: []string{testNamespace}}, engine.Spec.NamespaceSelector)
	assert.Equal(t, []podMetricsEndpoint{
		{Port: "metrics", Path: "/metrics", Scheme: "http", RelabelConfigs: identityRelabelConfigs("engine", "mysql")},
		{Port: "probe", Path: "/probe", Scheme: "http", RelabelConfigs: identityRelabelConfigs("engine", "mysql")},
	}, engine.Spec.PodMetricsEndpoints)
	assert.Equal(t, []podMetricsEndpoint{
		{PortNumber: 9216, Path: "/metrics", Scheme: "http", RelabelConfigs: identityRelabelConfigs("configServer", "mongodb")},
	}, configServer.Spec.PodMetricsEndpoints)
}

func TestPodMetricsEndpointFor(t *testing.T) {
	t.Parallel()

	credentials := []corev1alpha1.MonitoringCredential{
		{Profile: "metrics", SecretRef: common.SecretRef{Name: "orders-monitoring-metrics"}},
		{Profile: qanProfile, SecretRef: common.SecretRef{Name: "orders-monitoring-qan"}},
	}
	tests := []struct {
		name     string
		endpoint corev1alpha1.MetricsEndpoint
		params   destinationParameters
		want     podMetricsEndpoint
	}{
		{
			name:     "named port with path and scheme defaults",
			endpoint: corev1alpha1.MetricsEndpoint{Port: intstr.FromString("metrics")},
			want:     podMetricsEndpoint{Port: "metrics", Path: "/metrics", Scheme: "http"},
		},
		{
			name:     "numbered port, path and scheme as published",
			endpoint: corev1alpha1.MetricsEndpoint{Port: intstr.FromInt32(9104), Path: "/stats", Scheme: "https"},
			want:     podMetricsEndpoint{PortNumber: 9104, Path: "/stats", Scheme: "https"},
		},
		{
			name:     "scrape interval from the destination",
			endpoint: corev1alpha1.MetricsEndpoint{Port: intstr.FromString("metrics")},
			params:   destinationParameters{Interval: "30s"},
			want:     podMetricsEndpoint{Port: "metrics", Path: "/metrics", Scheme: "http", Interval: "30s"},
		},
		{
			name: "params copied verbatim",
			endpoint: corev1alpha1.MetricsEndpoint{
				Port:   intstr.FromString("metrics"),
				Params: map[string][]string{"collect[]": {"global_status", "info_schema.processlist"}},
			},
			want: podMetricsEndpoint{
				Port: "metrics", Path: "/metrics", Scheme: "http",
				Params: map[string][]string{"collect[]": {"global_status", "info_schema.processlist"}},
			},
		},
		{
			name: "tls with CA and server name",
			endpoint: corev1alpha1.MetricsEndpoint{
				Port:   intstr.FromString("metrics"),
				Scheme: "https",
				TLS: &corev1alpha1.MetricsEndpointTLS{
					CA:         &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "orders-ca"}, Key: "ca.crt"},
					ServerName: "orders-pxc.team-a.svc",
				},
			},
			want: podMetricsEndpoint{
				Port: "metrics", Path: "/metrics", Scheme: "https",
				TLSConfig: &tlsConfig{
					CA:         &secretOrConfigMap{Secret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "orders-ca"}, Key: "ca.crt"}},
					ServerName: "orders-pxc.team-a.svc",
				},
			},
		},
		{
			name: "tls without verification",
			endpoint: corev1alpha1.MetricsEndpoint{
				Port:   intstr.FromString("metrics"),
				Scheme: "https",
				TLS:    &corev1alpha1.MetricsEndpointTLS{InsecureSkipVerify: true},
			},
			want: podMetricsEndpoint{
				Port: "metrics", Path: "/metrics", Scheme: "https",
				TLSConfig: &tlsConfig{InsecureSkipVerify: true},
			},
		},
		{
			name: "basic auth from the Instance's credential profile",
			endpoint: corev1alpha1.MetricsEndpoint{
				Port: intstr.FromString("metrics"),
				Auth: &corev1alpha1.MetricsEndpointAuth{CredentialProfile: qanProfile},
			},
			want: podMetricsEndpoint{
				Port: "metrics", Path: "/metrics", Scheme: "http",
				BasicAuth: &basicAuth{
					Username: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "orders-monitoring-qan"}, Key: "username"},
					Password: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "orders-monitoring-qan"}, Key: "password"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.endpoint.Component, tt.endpoint.Kind = "engine", "mysql"
			sources := &corev1alpha1.MonitoringSources{Identity: testIdentity(), Credentials: credentials}
			tt.want.RelabelConfigs = identityRelabelConfigs("engine", "mysql")

			got, err := podMetricsEndpointFor(sources, tt.endpoint, tt.params)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPodMetricsEndpointForMultiTargetProbe(t *testing.T) {
	t.Parallel()
	sources := &corev1alpha1.MonitoringSources{Identity: corev1alpha1.MonitoringIdentity{Labels: map[string]string{"openeverest_instance": "orders"}}}
	ep := corev1alpha1.MetricsEndpoint{
		Component: "engine",
		Kind:      "mysql",
		Port:      intstr.FromInt32(9104),
		Path:      "/probe",
		Params:    map[string][]string{"target": {"mysql://127.0.0.1:3306"}},
	}

	got, err := podMetricsEndpointFor(sources, ep, destinationParameters{})
	require.NoError(t, err)
	assert.Equal(t, []relabelConfig{
		{TargetLabel: "openeverest_component", Replacement: new("engine"), Action: "replace"},
		{TargetLabel: "openeverest_instance", Replacement: new("orders"), Action: "replace"},
		{TargetLabel: "openeverest_kind", Replacement: new("mysql"), Action: "replace"},
		{SourceLabels: []string{"__param_target"}, TargetLabel: "instance", Action: "replace"},
	}, got.RelabelConfigs)
	assert.Equal(t, map[string][]string{"target": {"mysql://127.0.0.1:3306"}}, got.Params)
}

func TestPodMetricsEndpointForStampsOnlyContractIdentityLabels(t *testing.T) {
	t.Parallel()
	identity := testIdentity()
	identity.Labels["__address__"] = "collector.example.com:9100"
	identity.Labels["__scheme__"] = "https"
	identity.Labels["team"] = "dba"
	sources := &corev1alpha1.MonitoringSources{Identity: identity}
	ep := corev1alpha1.MetricsEndpoint{Component: "engine", Kind: "mysql", Port: intstr.FromString("metrics")}

	got, err := podMetricsEndpointFor(sources, ep, destinationParameters{})
	require.NoError(t, err)
	assert.Equal(t, identityRelabelConfigs("engine", "mysql"), got.RelabelConfigs)
}

func TestVMPodScrapesForRejectsUnrenderableSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		metrics []corev1alpha1.MetricsEndpoint
		wantErr string
	}{
		{
			name:    "endpoint without a port",
			metrics: []corev1alpha1.MetricsEndpoint{{Component: "engine", Kind: "mysql", PodSelector: engineSelector()}},
			wantErr: `component "engine": metrics endpoint has no port`,
		},
		{
			name: "credential profile the Instance does not publish",
			metrics: []corev1alpha1.MetricsEndpoint{{
				Component: "engine", Kind: "mysql", PodSelector: engineSelector(), Port: intstr.FromString("metrics"),
				Auth: &corev1alpha1.MetricsEndpointAuth{CredentialProfile: qanProfile},
			}},
			wantErr: `component "engine": credential profile "queryAnalytics" is not published by the Instance`,
		},
		{
			name: "endpoints of one component selecting different pods",
			metrics: []corev1alpha1.MetricsEndpoint{
				{Component: "engine", Kind: "mysql", PodSelector: engineSelector(), Port: intstr.FromString("metrics")},
				{Component: "engine", Kind: "mysql", PodSelector: map[string]string{monitoringv1alpha1.BindingInstanceLabel: "orders"}, Port: intstr.FromString("metrics")},
			},
			wantErr: `metrics endpoints of component "engine" select different pods`,
		},
		{
			name: "pod selector without the Instance label",
			metrics: []corev1alpha1.MetricsEndpoint{{
				Component: "engine", Kind: "mysql", PodSelector: map[string]string{"app.kubernetes.io/component": "pxc"}, Port: intstr.FromString("metrics"),
			}},
			wantErr: `component "engine": pod selector does not select core.openeverest.io/instance=orders`,
		},
		{
			name: "pod selector of another Instance",
			metrics: []corev1alpha1.MetricsEndpoint{{
				Component: "engine", Kind: "mysql", PodSelector: map[string]string{monitoringv1alpha1.BindingInstanceLabel: "billing"}, Port: intstr.FromString("metrics"),
			}},
			wantErr: `component "engine": pod selector does not select core.openeverest.io/instance=orders`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sources := &corev1alpha1.MonitoringSources{
				Identity:    testIdentity(),
				Credentials: []corev1alpha1.MonitoringCredential{{Profile: "metrics", SecretRef: common.SecretRef{Name: "orders-monitoring-metrics"}}},
				Metrics:     tt.metrics,
			}
			_, err := vmPodScrapesFor(testBinding(), "victoriametrics", sources, destinationParameters{})
			assert.EqualError(t, err, tt.wantErr)
		})
	}
}

func TestDecodeParameters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     *runtime.RawExtension
		want    destinationParameters
		wantErr string
	}{
		{name: "no parameters", raw: nil, want: destinationParameters{}},
		{
			name: "labels and interval",
			raw:  &runtime.RawExtension{Raw: []byte(`{"vmPodScrapeLabels":{"vmagent":"main","team.example.com/owner":"dba"},"interval":"1m30s"}`)},
			want: destinationParameters{VMPodScrapeLabels: map[string]string{"vmagent": "main", "team.example.com/owner": "dba"}, Interval: "1m30s"},
		},
		{
			name:    "labels of the wrong type",
			raw:     &runtime.RawExtension{Raw: []byte(`{"vmPodScrapeLabels":"vmagent=main"}`)},
			wantErr: "decode parameters",
		},
		{
			name:    "label naming the Instance",
			raw:     &runtime.RawExtension{Raw: []byte(`{"vmPodScrapeLabels":{"core.openeverest.io/instance":"payments"}}`)},
			wantErr: `vmPodScrapeLabels: key "core.openeverest.io/instance" is reserved for OpenEverest`,
		},
		{
			name:    "label in the monitoring API group",
			raw:     &runtime.RawExtension{Raw: []byte(`{"vmPodScrapeLabels":{"monitoring.openeverest.io/destination":"other"}}`)},
			wantErr: `vmPodScrapeLabels: key "monitoring.openeverest.io/destination" is reserved for OpenEverest`,
		},
		{
			name:    "invalid label key",
			raw:     &runtime.RawExtension{Raw: []byte(`{"vmPodScrapeLabels":{"not a key":"x"}}`)},
			wantErr: `vmPodScrapeLabels: invalid key "not a key"`,
		},
		{
			name:    "invalid label value",
			raw:     &runtime.RawExtension{Raw: []byte(`{"vmPodScrapeLabels":{"vmagent":"main agent"}}`)},
			wantErr: `vmPodScrapeLabels: invalid value for "vmagent"`,
		},
		{
			name:    "interval that is not a duration",
			raw:     &runtime.RawExtension{Raw: []byte(`{"interval":"30 seconds"}`)},
			wantErr: `interval "30 seconds" is not a Prometheus duration`,
		},
		{
			name:    "interval with units out of order",
			raw:     &runtime.RawExtension{Raw: []byte(`{"interval":"30s1m"}`)},
			wantErr: `interval "30s1m" is not a Prometheus duration`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeParameters(tt.raw)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

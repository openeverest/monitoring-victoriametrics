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

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestLeaseName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		controllerName string
		want           string
	}{
		{controllerName: "openeverest.io/monitoring-victoriametrics", want: "openeverest.io-monitoring-victoriametrics.monitoring-victoriametrics.openeverest.io"},
		{controllerName: "example.com/victoriametrics_eu", want: "example.com-victoriametrics-eu.monitoring-victoriametrics.openeverest.io"},
	}
	for _, tt := range tests {
		t.Run(tt.controllerName, func(t *testing.T) {
			t.Parallel()
			got := leaseName(tt.controllerName)
			assert.Equal(t, tt.want, got)
			assert.Empty(t, validation.IsDNS1123Subdomain(got))
		})
	}
}

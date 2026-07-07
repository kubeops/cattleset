/*
Copyright AppsCode Inc. and Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gate

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// gateFlipsTotal counts admission flips by result (admitted or revoked), so the
// gate admit/revoke rate is observable.
var gateFlipsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "cattleset_gate_flips_total",
	Help: "Total write-path gate flips, by result (admitted or revoked).",
}, []string{"namespace", "service", "result"})

func init() {
	metrics.Registry.MustRegister(gateFlipsTotal)
}

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

package cattleset

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// postureFlipsTotal counts primary-DC (holder) changes realized per herd.
	postureFlipsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cattleset_posture_flips_total",
		Help: "Total number of primary-DC posture flips realized, per CattleSet.",
	}, []string{"namespace", "name"})

	// observeToConvergeSeconds measures the time from a holder change to the
	// herd reporting PostureConverged.
	observeToConvergeSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cattleset_observe_to_converge_seconds",
		Help:    "Seconds from a Lease holder change to PostureConverged, per CattleSet.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 10),
	}, []string{"namespace", "name"})

	// siteReady is the per-site ready replica gauge.
	siteReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cattleset_site_ready_replicas",
		Help: "Ready replicas per site, per CattleSet.",
	}, []string{"namespace", "name", "cluster"})

	// siteDesired is the per-site desired replica gauge.
	siteDesired = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cattleset_site_desired_replicas",
		Help: "Desired replicas per site, per CattleSet.",
	}, []string{"namespace", "name", "cluster"})
)

func init() {
	metrics.Registry.MustRegister(postureFlipsTotal, observeToConvergeSeconds, siteReady, siteDesired)
}

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

package server

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"time"

	cattlesetctrl "kubeops.dev/cattleset/pkg/controller/cattleset"
	gateagent "kubeops.dev/cattleset/pkg/controller/gate"
	webhooks "kubeops.dev/cattleset/pkg/webhooks/apps/v1"

	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"kmodules.xyz/client-go/apiextensions"
	apiworkv1 "open-cluster-management.io/api/work/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

var setupLog = ctrl.Log.WithName("setup")

type OperatorConfig struct {
	ClientConfig *rest.Config

	KubeClient kubernetes.Interface

	ResyncPeriod   time.Duration
	MaxNumRequeues int
	NumThreads     int

	MetricsAddr          string
	CertDir              string
	EnableLeaderElection bool
	ProbeAddr            string
	SecureMetrics        bool
	EnableHTTP2          bool
	EnableGateAgent      bool
}

// New returns a new CattleSet operator manager from the given config.
func (c *OperatorConfig) New(ctx context.Context) (manager.Manager, error) {
	ctrl.SetLogger(klog.NewKlogr())
	var tlsOpts []func(*tls.Config)

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs.
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !c.EnableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for certificates
	var certWatcher *certwatcher.CertWatcher

	// Initial TLS options
	webhookTLSOpts := tlsOpts
	metricsTLSOpts := tlsOpts

	if len(c.CertDir) > 0 {
		setupLog.Info("Initializing certificate watcher using provided certificates",
			"cert-dir", c.CertDir, "cert-name", core.TLSCertKey, "cert-key", core.TLSPrivateKeyKey)

		var err error
		certWatcher, err = certwatcher.New(
			filepath.Join(c.CertDir, core.TLSCertKey),
			filepath.Join(c.CertDir, core.TLSPrivateKeyKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = certWatcher.GetCertificate
		})

		metricsTLSOpts = append(metricsTLSOpts, func(config *tls.Config) {
			config.GetCertificate = certWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	metricsServerOptions := metricsserver.Options{
		BindAddress:   c.MetricsAddr,
		SecureServing: c.SecureMetrics,
		TLSOpts:       metricsTLSOpts,
	}

	if c.SecureMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	mgr, err := ctrl.NewManager(c.ClientConfig, ctrl.Options{
		Scheme:                 Scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: c.ProbeAddr,
		LeaderElection:         c.EnableLeaderElection,
		LeaderElectionID:       "cattleset.k8s.appscode.com",
		Cache: cache.Options{
			SyncPeriod: &c.ResyncPeriod, // Default SyncPeriod is 10 Hours
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		if err := apiextensions.NewReconciler(ctx, mgr).SetupWithManager(mgr); err != nil {
			return errors.Wrap(err, "unable to create CustomResourceDefinition reconciler")
		}
		apiextensions.RegisterSetup(schema.GroupKind{
			Group: apiworkv1.GroupName,
			Kind:  "ManifestWork",
		}, func(ctx context.Context, mgr manager.Manager) {})
		return nil
	})); err != nil {
		setupLog.Error(err, "unable to set up CustomResourceDefinition reconciler")
		os.Exit(1)
	}

	if err = (&cattlesetctrl.Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		// A consistent, uncached reader for Lease reads: a posture decision must
		// never come from a cached list, and it fails closed.
		Holder: &cattlesetctrl.LeaseFollower{Reader: mgr.GetAPIReader()},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CattleSet")
		os.Exit(1)
	}

	if c.EnableGateAgent {
		if err = (&gateagent.Agent{
			Client:   mgr.GetClient(),
			Recorder: mgr.GetEventRecorderFor("cattleset-gate-agent"),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "GateAgent")
			os.Exit(1)
		}
	}

	if err = webhooks.SetupCattleSetWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "CattleSet")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if certWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(certWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	return mgr, nil
}

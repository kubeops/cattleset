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

	api "kubeops.dev/cattleset/apis/apps/v1"
	cattlesetctrl "kubeops.dev/cattleset/pkg/controller/cattleset"
	gateagent "kubeops.dev/cattleset/pkg/controller/gate"
	webhooks "kubeops.dev/cattleset/pkg/webhooks/apps/v1"

	"github.com/pkg/errors"
	coordinationv1 "k8s.io/api/coordination/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"kmodules.xyz/client-go/apiextensions"
	apiworkv1 "open-cluster-management.io/api/work/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
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

	LeaderElectionID    string
	EnableHubController bool
	EnableGateAgent     bool
	DCName              string
	ClusterName         string
	CoordKubeconfig     string
	ArbiterHub          bool
	GateResync          time.Duration
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
		LeaderElectionID:       c.LeaderElectionID,
		Cache:                  c.cacheOptions(),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if c.EnableHubController {
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

		// The coordination plane is read through a consistent, uncached reader, and
		// watched through its own cache for the events that matter. It is read only.
		coordReader, coordCache, err := c.coordination(mgr)
		if err != nil {
			setupLog.Error(err, "unable to connect to the coordination control plane")
			os.Exit(1)
		}
		if err = (&cattlesetctrl.Reconciler{
			Client:           mgr.GetClient(),
			Scheme:           mgr.GetScheme(),
			Recorder:         mgr.GetEventRecorderFor("cattleset-controller"),
			Reader:           mgr.GetAPIReader(),
			Coord:            &cattlesetctrl.LeaseFollower{Reader: coordReader},
			LocalClusterName: c.ClusterName,
		}).SetupWithManager(mgr, coordCache); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "CattleSet")
			os.Exit(1)
		}

		if err = webhooks.SetupCattleSetWebhookWithManager(mgr, c.ArbiterHub); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "CattleSet")
			os.Exit(1)
		}
	}

	if c.EnableGateAgent {
		if err = (&gateagent.Agent{
			Client:   mgr.GetClient(),
			Reader:   mgr.GetAPIReader(),
			Recorder: mgr.GetEventRecorderFor("cattleset-gate-agent"),
			DC:       c.DCName,
			Resync:   c.GateResync,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "GateAgent")
			os.Exit(1)
		}
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

// cacheOptions scopes what each role watches. The hub controller needs coordination
// Leases only from dc-failover. A gate agent runs on a spoke it does not own, so
// it caches only its herds' Services and the marker ConfigMaps in dc-failover:
// not every ConfigMap and Service in the cluster.
func (c *OperatorConfig) cacheOptions() cache.Options {
	opts := cache.Options{
		SyncPeriod: &c.ResyncPeriod, // Default SyncPeriod is 10 Hours
		ByObject: map[client.Object]cache.ByObject{
			&coordinationv1.Lease{}: {Namespaces: map[string]cache.Config{api.CoordNamespace: {}}},
		},
	}
	if !c.EnableHubController && c.EnableGateAgent {
		herd, _ := labels.NewRequirement(api.CattleSetNameLabel, selection.Exists, nil)
		opts.ByObject[&core.Service{}] = cache.ByObject{Label: labels.NewSelector().Add(*herd)}
		opts.ByObject[&core.ConfigMap{}] = cache.ByObject{Namespaces: map[string]cache.Config{api.CoordNamespace: {}}}
	}
	return opts
}

// coordination returns a consistent reader for, and the cache of, the
// coordination control plane. With no kubeconfig it is the hub's own API server.
// A separate coordination plane is connected read only.
func (c *OperatorConfig) coordination(mgr manager.Manager) (client.Reader, cache.Cache, error) {
	if c.CoordKubeconfig == "" {
		return mgr.GetAPIReader(), mgr.GetCache(), nil
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", c.CoordKubeconfig)
	if err != nil {
		return nil, nil, errors.Wrap(err, "reading --coord-kubeconfig")
	}
	cl, err := cluster.New(cfg, func(o *cluster.Options) {
		o.Scheme = Scheme
		o.Cache = cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&coordinationv1.Lease{}: {Namespaces: map[string]cache.Config{api.CoordNamespace: {}}},
			},
		}
	})
	if err != nil {
		return nil, nil, err
	}
	if err := mgr.Add(cl); err != nil {
		return nil, nil, err
	}
	return cl.GetAPIReader(), cl.GetCache(), nil
}

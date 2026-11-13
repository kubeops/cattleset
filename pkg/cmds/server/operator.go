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
	"errors"
	"flag"
	"time"

	api "kubeops.dev/cattleset/apis/apps/v1"
	gateagent "kubeops.dev/cattleset/pkg/controller/gate"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	"github.com/spf13/pflag"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientsetscheme "k8s.io/client-go/kubernetes/scheme"
	apiworkv1 "open-cluster-management.io/api/work/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var Scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientsetscheme.AddToScheme(Scheme))
	utilruntime.Must(apiworkv1.AddToScheme(Scheme))
	utilruntime.Must(api.AddToScheme(Scheme))
	utilruntime.Must(petsetapi.AddToScheme(Scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(Scheme))
	utilruntime.Must(gwv1.Install(Scheme))
}

type OperatorOptions struct {
	QPS   float64
	Burst int

	ResyncPeriod   time.Duration
	MaxNumRequeues int
	NumThreads     int

	MetricsAddr          string
	CertDir              string
	EnableLeaderElection bool
	ProbeAddr            string
	SecureMetrics        bool
	EnableHTTP2          bool

	// LeaderElectionID names the leader election Lease. The hub controller and the
	// per-spoke gate agent are separate Deployments and need different ones.
	LeaderElectionID string

	// EnableHubController runs the CattleSet reconciler and the admission webhook.
	// The hub install runs only this.
	EnableHubController bool

	// EnableGateAgent runs the spoke gate agent. In production it is its own
	// Deployment on every spoke (with the hub controller off); both can run in one
	// process for single-cluster development.
	EnableGateAgent bool

	// DCName is this spoke's name, the OCM managed cluster name, the same string the
	// scope markers carry in activeDC. Required for the gate agent.
	DCName string

	// ClusterName is the OCM managed cluster name of the cluster the hub controller
	// runs in. When it is not a Member of a herd's placement policy (the stretched
	// hub is the Arbiter DC), the herd's hub-local workload is retired rather than
	// adopted.
	ClusterName string

	// CoordKubeconfig is the kubeconfig of the coordination control plane, the API
	// server that serves the Leases in dc-failover. Empty means the hub's own.
	CoordKubeconfig string

	// ArbiterHub says this hub runs no workloads: creating a non-distributed
	// CattleSet on it is warned about.
	ArbiterHub bool

	// GateResync is how often the gate agent re-evaluates a Service.
	GateResync time.Duration
}

func NewOperatorOptions() *OperatorOptions {
	return &OperatorOptions{
		// High enough QPS to fit all expected use cases. QPS=0 is not set here, because client code is overriding it.
		QPS: 1e6,
		// High enough Burst to fit all expected use cases. Burst=0 is not set here, because client code is overriding it.
		Burst:          1e6,
		ResyncPeriod:   10 * time.Minute,
		MaxNumRequeues: 5,
		NumThreads:     5,
		MetricsAddr:    "0",
		ProbeAddr:      ":8081",
		// The hub controller is HA: two or more replicas, one leader.
		EnableLeaderElection: true,
		LeaderElectionID:     "cattleset.k8s.appscode.com",
		SecureMetrics:        true,
		CertDir:              "",
		EnableHTTP2:          false,
		EnableHubController:  true,
		// The agent is its own Deployment on each spoke, not part of the hub.
		EnableGateAgent: false,
		GateResync:      gateagent.DefaultResync,
	}
}

func (s *OperatorOptions) AddGoFlags(fs *flag.FlagSet) {
	fs.Float64Var(&s.QPS, "qps", s.QPS, "The maximum QPS to the master from this client")
	fs.IntVar(&s.Burst, "burst", s.Burst, "The maximum burst for throttle")

	fs.DurationVar(&s.ResyncPeriod, "resync-period", s.ResyncPeriod, "If non-zero, will re-list this often. Otherwise, re-list will be delayed as long as possible (until the upstream source closes the watch or times out.")
	fs.IntVar(&s.NumThreads, "max-concurrent-reconciles", s.NumThreads, "The maximum number of concurrent reconciles which can be run")

	fs.StringVar(&s.MetricsAddr, "metrics-bind-address", s.MetricsAddr, "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	fs.StringVar(&s.ProbeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	fs.BoolVar(&s.EnableLeaderElection, "leader-elect", s.EnableLeaderElection,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	fs.BoolVar(&s.SecureMetrics, "metrics-secure", s.SecureMetrics,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	fs.StringVar(&s.CertDir, "cert-dir", s.CertDir, "The directory that contains the webhook and metrics server certificate.")
	fs.BoolVar(&s.EnableHTTP2, "enable-http2", s.EnableHTTP2,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	fs.StringVar(&s.LeaderElectionID, "leader-election-id", s.LeaderElectionID, "Name of the leader election Lease. Use a different one for the gate agent.")
	fs.BoolVar(&s.EnableHubController, "hub-controller", s.EnableHubController,
		"Run the CattleSet reconciler and webhook. Turn off for a spoke gate agent deployment.")
	fs.BoolVar(&s.EnableGateAgent, "gate-agent", s.EnableGateAgent,
		"Run the spoke gate agent. In production it is its own Deployment on every spoke, with --hub-controller=false.")
	fs.StringVar(&s.DCName, "dc-name", s.DCName, "This spoke's OCM managed cluster name (the DC name in the scope markers). Required for the gate agent.")
	fs.StringVar(&s.ClusterName, "cluster-name", s.ClusterName, "The OCM managed cluster name of the cluster the hub controller runs in. When it is not a Member of a herd, its hub-local workload is retired instead of adopted.")
	fs.StringVar(&s.CoordKubeconfig, "coord-kubeconfig", s.CoordKubeconfig, "Kubeconfig of the coordination control plane that serves the dc-failover Leases. Empty means the hub's own API server. Read only.")
	fs.BoolVar(&s.ArbiterHub, "arbiter-hub", s.ArbiterHub, "This hub is an Arbiter DC and runs no workloads: warn when a non-distributed CattleSet is created on it.")
	fs.DurationVar(&s.GateResync, "gate-resync", s.GateResync, "How often the gate agent re-evaluates a gated Service.")
}

func (s *OperatorOptions) AddFlags(fs *pflag.FlagSet) {
	pfs := flag.NewFlagSet("extra-flags", flag.ExitOnError)
	s.AddGoFlags(pfs)
	fs.AddGoFlagSet(pfs)
}

func (s *OperatorOptions) ApplyTo(cfg *OperatorConfig) error {
	var err error

	cfg.ClientConfig.QPS = float32(s.QPS)
	cfg.ClientConfig.Burst = s.Burst
	cfg.ResyncPeriod = s.ResyncPeriod
	cfg.MaxNumRequeues = s.MaxNumRequeues
	cfg.NumThreads = s.NumThreads

	if cfg.KubeClient, err = kubernetes.NewForConfig(cfg.ClientConfig); err != nil {
		return err
	}
	cfg.MetricsAddr = s.MetricsAddr
	cfg.ProbeAddr = s.ProbeAddr
	cfg.EnableLeaderElection = s.EnableLeaderElection
	cfg.SecureMetrics = s.SecureMetrics
	cfg.CertDir = s.CertDir
	cfg.EnableHTTP2 = s.EnableHTTP2
	cfg.LeaderElectionID = s.LeaderElectionID
	cfg.EnableHubController = s.EnableHubController
	cfg.EnableGateAgent = s.EnableGateAgent
	cfg.DCName = s.DCName
	cfg.ClusterName = s.ClusterName
	cfg.CoordKubeconfig = s.CoordKubeconfig
	cfg.ArbiterHub = s.ArbiterHub
	cfg.GateResync = s.GateResync

	return nil
}

func (s *OperatorOptions) Validate() []error {
	var errs []error
	if !s.EnableHubController && !s.EnableGateAgent {
		errs = append(errs, errors.New("nothing to run: enable --hub-controller, --gate-agent, or both"))
	}
	if s.EnableGateAgent && s.DCName == "" {
		// An agent that does not know which DC it is would compare every marker
		// against the empty string and never admit; refuse to start instead.
		errs = append(errs, errors.New("--dc-name is required with --gate-agent"))
	}
	return errs
}

func (s *OperatorOptions) Complete() error {
	return nil
}

func (s OperatorOptions) Config() (*OperatorConfig, error) {
	clientConfig, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}

	cfg := &OperatorConfig{
		ClientConfig: clientConfig,
	}
	if err := s.ApplyTo(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (s OperatorOptions) Run(ctx context.Context) error {
	cfg, err := s.Config()
	if err != nil {
		return err
	}

	mgr, err := cfg.New(ctx)
	if err != nil {
		return err
	}
	return mgr.Start(ctx)
}

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
	"context"
	"time"

	api "kubeops.dev/cattleset/apis/apps/v1"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// primaryLeaseName is the primary DC Lease of a FailoverGroup, in dr-controlplane's
// naming: primary-dc-<group>, or the global primary-dc for no group.
func primaryLeaseName(group string) string {
	if group != "" {
		return api.GroupPrimaryPrefix + group
	}
	return api.GlobalPrimaryLease
}

func healthLeaseName(dc string) string { return api.HealthLeasePrefix + dc }

// LeaseObservation is what one read of a coordination Lease tells the controller.
// It keeps "I could not read it" apart from "it says nobody holds it": capacity
// must hold on the first and may react to the second.
type LeaseObservation struct {
	// Found is true when the Lease exists and was read.
	Found bool
	// Err is a read error other than not-found. The Lease's state is then unknown.
	Err error
	// Holder is spec.holderIdentity.
	Holder string
	// Fresh is true when renewTime plus leaseDurationSeconds is in the future.
	Fresh bool
	// Expires is that deadline; zero when it cannot be established.
	Expires time.Time
	// Annotations are the Lease's annotations (follow-dc, handoff-to, ...).
	Annotations map[string]string
}

// HolderKnown is true only for a Lease that was read, is unexpired, and names a
// holder. Anything else means the primary DC is not established right now.
func (o LeaseObservation) HolderKnown() bool {
	return o.Found && o.Err == nil && o.Fresh && o.Holder != ""
}

// Health classifies a DC health Lease read.
type Health int

const (
	// HealthUnknown: the read failed. Nothing may be concluded.
	HealthUnknown Health = iota
	// HealthAlive: the Lease is fresh.
	HealthAlive
	// HealthDead: the Lease exists and expired. The DC stopped renewing it.
	HealthDead
	// HealthAbsent: there is no such Lease (a cluster without DC-DR coordination).
	HealthAbsent
)

// Health interprets a health Lease observation.
func (o LeaseObservation) Health() Health {
	switch {
	case o.Err != nil:
		return HealthUnknown
	case !o.Found:
		return HealthAbsent
	case o.Fresh:
		return HealthAlive
	default:
		return HealthDead
	}
}

// Coordination reads the Leases dr-controlplane publishes on the coordination
// control plane. Implementations must read consistently: a posture decision must
// never come from a stale cached list.
type Coordination interface {
	Lease(ctx context.Context, name string) LeaseObservation
}

// LeaseFollower reads Leases through a consistent (uncached) reader. It does not
// decide anything: it never turns a failed read into "no holder".
//
// +kubebuilder:object:generate=false
type LeaseFollower struct {
	Reader    client.Reader
	Namespace string
	// Now is injectable for tests.
	Now func() time.Time
}

func (f *LeaseFollower) namespace() string {
	if f.Namespace == "" {
		return api.CoordNamespace
	}
	return f.Namespace
}

func (f *LeaseFollower) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Lease implements Coordination.
func (f *LeaseFollower) Lease(ctx context.Context, name string) LeaseObservation {
	l := &coordinationv1.Lease{}
	if err := f.Reader.Get(ctx, client.ObjectKey{Namespace: f.namespace(), Name: name}, l); err != nil {
		if apierrors.IsNotFound(err) {
			return LeaseObservation{}
		}
		return LeaseObservation{Err: err}
	}
	o := LeaseObservation{Found: true, Annotations: l.Annotations}
	if l.Spec.HolderIdentity != nil {
		o.Holder = *l.Spec.HolderIdentity
	}
	if l.Spec.RenewTime != nil && l.Spec.LeaseDurationSeconds != nil {
		o.Expires = l.Spec.RenewTime.Add(time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second)
		o.Fresh = f.now().Before(o.Expires)
	}
	return o
}

var _ Coordination = (*LeaseFollower)(nil)

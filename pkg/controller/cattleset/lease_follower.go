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
	"fmt"
	"time"

	api "kubeops.dev/cattleset/apis/apps/v1"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const defaultLeaseNamespace = "kube-system"

// LeaseFollower resolves the primary DC by reading the shared primary-DC Lease
// the referenced PlacementPolicy's failoverPolicy.trigger names, and the per-DC
// health Leases that feed the ActiveActive surge. It follows the same Lease the
// database tier does (design Section 5.3).
//
// It implements PrimaryDCResolver. Reads use a consistent (uncached) reader and
// fail closed: an unreadable or expired Lease means not-primary, so a partitioned
// site's herd is treated as standby. The Lease's leaseDurationSeconds is the only
// hysteresis, so there is no second grace knob.
//
// +kubebuilder:object:generate=false
type LeaseFollower struct {
	// Reader is a consistent (uncached) reader; a posture decision must never
	// come from a cached list.
	Reader client.Reader

	// Namespace holds the coordination Leases (default kube-system).
	Namespace string

	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

func (f *LeaseFollower) namespace() string {
	if f.Namespace == "" {
		return defaultLeaseNamespace
	}
	return f.Namespace
}

func (f *LeaseFollower) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// primaryLeaseName derives the primary-DC Lease name from the trigger. Group
// scope follows primary-dc-<group>; Global follows the single primary-dc-global.
func primaryLeaseName(t petsetapi.FailoverTrigger) string {
	if t.Scope == petsetapi.FailoverScopeGroup && t.Group != "" {
		return "primary-dc-" + t.Group
	}
	return "primary-dc-global"
}

func healthLeaseName(cluster string) string {
	return "dc-health-" + cluster
}

// PrimaryDC reads the trigger from the herd's PlacementPolicy, then the
// primary-DC Lease, and returns its holderIdentity plus the set of Members whose
// health Lease is stale. It fails closed on any read error or an expired Lease.
func (f *LeaseFollower) PrimaryDC(ctx context.Context, cs *api.CattleSet) (string, map[string]bool, error) {
	if cs.Spec.PodPlacementPolicy == nil {
		return "", nil, nil
	}
	pp := &petsetapi.PlacementPolicy{}
	if err := f.Reader.Get(ctx, client.ObjectKey{Name: cs.Spec.PodPlacementPolicy.Name}, pp); err != nil {
		return "", nil, client.IgnoreNotFound(err)
	}
	csc := pp.Spec.ClusterSpreadConstraint
	if csc == nil || csc.FailoverPolicy == nil {
		// No trigger to follow (a pure ActiveActive herd): no holder, and health
		// staleness still matters for the surge.
		return "", f.staleMembers(ctx, csc), nil
	}

	holder := f.readHolder(ctx, primaryLeaseName(csc.FailoverPolicy.Trigger))
	return holder, f.staleMembers(ctx, csc), nil
}

// readHolder returns the Lease holderIdentity, or "" (fail closed) when the
// Lease cannot be read or has expired.
func (f *LeaseFollower) readHolder(ctx context.Context, name string) string {
	lease := &coordinationv1.Lease{}
	if err := f.Reader.Get(ctx, client.ObjectKey{Namespace: f.namespace(), Name: name}, lease); err != nil {
		return ""
	}
	if f.expired(lease) {
		return ""
	}
	if lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

func (f *LeaseFollower) staleMembers(ctx context.Context, csc *petsetapi.ClusterSpreadConstraint) map[string]bool {
	if csc == nil {
		return nil
	}
	stale := map[string]bool{}
	for _, r := range csc.DistributionRules {
		if effectiveRole(r.Role) != petsetapi.DCRoleMember {
			continue
		}
		lease := &coordinationv1.Lease{}
		err := f.Reader.Get(ctx, client.ObjectKey{Namespace: f.namespace(), Name: healthLeaseName(r.ClusterName)}, lease)
		if apierrors.IsNotFound(err) {
			// No health Lease at all: treat as healthy (nothing to surge for).
			continue
		}
		if err != nil || f.expired(lease) {
			stale[r.ClusterName] = true
		}
	}
	if len(stale) == 0 {
		return nil
	}
	return stale
}

// expired reports whether a Lease's renewTime plus leaseDurationSeconds is in
// the past. The lease duration is the hysteresis window (no second grace knob).
func (f *LeaseFollower) expired(lease *coordinationv1.Lease) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true // cannot establish freshness: fail closed
	}
	deadline := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	return f.now().After(deadline)
}

var _ PrimaryDCResolver = (*LeaseFollower)(nil)

// String identifies the follower for logs.
func (f *LeaseFollower) String() string {
	return fmt.Sprintf("LeaseFollower(namespace=%s)", f.namespace())
}

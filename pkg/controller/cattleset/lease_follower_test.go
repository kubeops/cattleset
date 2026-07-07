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
	"testing"
	"time"

	api "kubeops.dev/cattleset/apis/apps/v1"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	coordinationv1 "k8s.io/api/coordination/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func leaseScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := coordinationv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := petsetapi.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

var fixedNow = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

func lease(name, holder string, ageSeconds, durationSeconds int32) *coordinationv1.Lease {
	renew := metav1.NewMicroTime(fixedNow.Add(-time.Duration(ageSeconds) * time.Second))
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: defaultLeaseNamespace},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr.To(holder),
			LeaseDurationSeconds: ptr.To(durationSeconds),
			RenewTime:            &renew,
		},
	}
}

func apHerd() (*api.CattleSet, *petsetapi.PlacementPolicy) {
	cs := &api.CattleSet{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-api", Namespace: "orders"},
		Spec: api.CattleSetSpec{
			Distributed:        true,
			PodPlacementPolicy: &core.LocalObjectReference{Name: "orders-geo"},
			Topology:           &api.CattleTopology{Mode: api.CattleModeActivePassive},
		},
	}
	pp := &petsetapi.PlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-geo"},
		Spec: petsetapi.PlacementPolicySpec{
			ClusterSpreadConstraint: &petsetapi.ClusterSpreadConstraint{
				DistributionRules: []petsetapi.DistributionRule{
					{ClusterName: "dc-a", Role: petsetapi.DCRoleMember},
					{ClusterName: "dc-b", Role: petsetapi.DCRoleMember},
				},
				FailoverPolicy: &petsetapi.FailoverPolicy{
					Trigger: petsetapi.FailoverTrigger{Scope: petsetapi.FailoverScopeGroup, Group: "orders"},
				},
			},
		},
	}
	return cs, pp
}

func follower(t *testing.T, objs ...client.Object) *LeaseFollower {
	c := fake.NewClientBuilder().WithScheme(leaseScheme(t)).WithObjects(objs...).Build()
	return &LeaseFollower{Reader: c, Now: func() time.Time { return fixedNow }}
}

func TestLeaseFollowerHolder(t *testing.T) {
	cs, pp := apHerd()
	f := follower(t, pp, lease("primary-dc-orders", "dc-a", 5, 30))

	holder, _, err := f.PrimaryDC(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	if holder != "dc-a" {
		t.Errorf("holder = %q, want dc-a", holder)
	}
}

func TestLeaseFollowerHolderFlip(t *testing.T) {
	cs, pp := apHerd()
	f := follower(t, pp, lease("primary-dc-orders", "dc-b", 1, 30))

	holder, _, _ := f.PrimaryDC(context.Background(), cs)
	if holder != "dc-b" {
		t.Errorf("after flip holder = %q, want dc-b", holder)
	}
}

func TestLeaseFollowerExpiredFailsClosed(t *testing.T) {
	cs, pp := apHerd()
	// renewed 60s ago with a 30s duration: expired.
	f := follower(t, pp, lease("primary-dc-orders", "dc-a", 60, 30))

	holder, _, _ := f.PrimaryDC(context.Background(), cs)
	if holder != "" {
		t.Errorf("expired Lease holder = %q, want empty (fail closed)", holder)
	}
}

func TestLeaseFollowerMissingFailsClosed(t *testing.T) {
	cs, pp := apHerd()
	f := follower(t, pp) // no Lease at all

	holder, _, _ := f.PrimaryDC(context.Background(), cs)
	if holder != "" {
		t.Errorf("missing Lease holder = %q, want empty (fail closed)", holder)
	}
}

func TestLeaseFollowerHealthStaleness(t *testing.T) {
	cs, pp := apHerd()
	f := follower(t, pp,
		lease("primary-dc-orders", "dc-a", 5, 30),
		lease("dc-health-dc-a", "dc-a", 5, 30),  // fresh
		lease("dc-health-dc-b", "dc-b", 90, 30), // stale
	)

	_, stale, _ := f.PrimaryDC(context.Background(), cs)
	if !stale["dc-b"] {
		t.Errorf("dc-b should be stale, stale=%v", stale)
	}
	if stale["dc-a"] {
		t.Errorf("dc-a should be fresh, stale=%v", stale)
	}
}

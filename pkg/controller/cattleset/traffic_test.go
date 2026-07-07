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
	"testing"

	api "kubeops.dev/cattleset/apis/apps/v1"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func trafficHerd() *api.CattleSet {
	cs := distributedHerd()
	cs.Spec.Traffic = &api.CattleTraffic{
		ServiceName: "orders-api",
		Export:      true,
		GSLB:        &api.CattleGSLB{Host: "orders.bank.example", TTLSeconds: ptr.To[int32](30)},
		HTTPRoute: &api.CattleHTTPRoute{
			ParentRefs: []gwv1.ParentReference{{Name: "public-gw"}},
		},
	}
	return cs
}

func TestRenderHTTPRouteNoRepoint(t *testing.T) {
	cs := trafficHerd()

	before := renderHTTPRoute(cs)
	if before == nil {
		t.Fatal("expected an HTTPRoute")
	}
	// hostnames default to the GSLB host.
	if len(before.Spec.Hostnames) != 1 || before.Spec.Hostnames[0] != "orders.bank.example" {
		t.Errorf("hostnames = %v, want [orders.bank.example]", before.Spec.Hostnames)
	}
	// backend is the local Service.
	be := before.Spec.Rules[0].BackendRefs[0]
	if string(be.Name) != "orders-api" {
		t.Errorf("backend = %q, want orders-api", be.Name)
	}

	// A failover changes the holder but the HTTPRoute is unchanged: it binds to
	// the local Gateway and Service, never repointed.
	after := renderHTTPRoute(cs)
	if diff := cmp.Diff(before.Spec, after.Spec); diff != "" {
		t.Errorf("HTTPRoute changed across a failover (-before +after):\n%s", diff)
	}
}

func gslbStrategy(t *testing.T, u *unstructured.Unstructured) map[string]any {
	t.Helper()
	s, found, err := unstructured.NestedMap(u.Object, "spec", "strategy")
	if err != nil || !found {
		t.Fatalf("no spec.strategy: %v", err)
	}
	return s
}

func TestRenderGslbActivePassiveFollowsHolder(t *testing.T) {
	cs := trafficHerd()

	a := renderGslb(cs, api.CattleModeActivePassive, "dc-a")
	if got := gslbStrategy(t, a); got["type"] != "failover" || got["primaryGeoTag"] != "dc-a" {
		t.Errorf("ActivePassive strategy = %v, want failover/dc-a", got)
	}

	// Holder flip retags the Gslb only.
	b := renderGslb(cs, api.CattleModeActivePassive, "dc-b")
	if got := gslbStrategy(t, b); got["primaryGeoTag"] != "dc-b" {
		t.Errorf("after flip primaryGeoTag = %v, want dc-b", got["primaryGeoTag"])
	}
}

func TestRenderGslbActiveActiveRoundRobin(t *testing.T) {
	cs := trafficHerd()
	u := renderGslb(cs, api.CattleModeActiveActive, "")
	if got := gslbStrategy(t, u); got["type"] != "roundRobin" {
		t.Errorf("ActiveActive strategy = %v, want roundRobin", got)
	}
}

func TestRenderServiceExportSlice(t *testing.T) {
	cs := trafficHerd()
	pp := &petsetapi.PlacementPolicy{
		Spec: petsetapi.PlacementPolicySpec{
			ClusterSpreadConstraint: &petsetapi.ClusterSpreadConstraint{
				Slice: petsetapi.KubeSliceConfig{SliceName: "orders-slice"},
			},
		},
	}
	u := renderServiceExport(cs, pp)
	if u == nil {
		t.Fatal("expected a ServiceExport")
	}
	slice, _, _ := unstructured.NestedString(u.Object, "spec", "slice")
	if slice != "orders-slice" {
		t.Errorf("slice = %q, want orders-slice", slice)
	}
	if u.GetKind() != "ServiceExport" {
		t.Errorf("kind = %q", u.GetKind())
	}
}

func TestRenderTrafficDisabledWhenUnset(t *testing.T) {
	cs := distributedHerd() // Traffic set but no GSLB/HTTPRoute/Export
	cs.Spec.Traffic = &api.CattleTraffic{ServiceName: "orders-api"}
	if renderHTTPRoute(cs) != nil {
		t.Error("no HTTPRoute expected")
	}
	if renderGslb(cs, api.CattleModeActivePassive, "dc-a") != nil {
		t.Error("no Gslb expected")
	}
	if renderServiceExport(cs, nil) != nil {
		t.Error("no ServiceExport expected")
	}
}

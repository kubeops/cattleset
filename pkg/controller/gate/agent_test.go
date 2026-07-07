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
	"context"
	"encoding/json"
	"testing"

	api "kubeops.dev/cattleset/apis/apps/v1"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kmapi "kmodules.xyz/client-go/api/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestIsPromotedHere(t *testing.T) {
	mk := func(dr map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{}}
		if dr != nil {
			_ = unstructured.SetNestedMap(u.Object, dr, "status", "disasterRecovery")
		}
		return u
	}
	tests := []struct {
		name string
		u    *unstructured.Unstructured
		want bool
	}{
		{"no status", mk(nil), false},
		{"promoted bool true", mk(map[string]any{"promoted": true}), true},
		{"promoted bool false", mk(map[string]any{"promoted": false}), false},
		{"role primary", mk(map[string]any{"role": "Primary"}), true},
		{"mode active", mk(map[string]any{"mode": "active"}), true},
		{"status standby", mk(map[string]any{"status": "Standby"}), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPromotedHere(tc.u); got != tc.want {
				t.Errorf("isPromotedHere = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDecideAdmit(t *testing.T) {
	tests := []struct {
		name   string
		states []tierState
		want   bool
	}{
		{"empty is not admitted", nil, false},
		{"all promoted", []tierState{{Present: true, Promoted: true}, {Present: true, Promoted: true}}, true},
		{"one demoted", []tierState{{Present: true, Promoted: true}, {Present: true, Promoted: false}}, false},
		{"one absent", []tierState{{Present: true, Promoted: true}, {Present: false}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideAdmit(tc.states); got != tc.want {
				t.Errorf("decideAdmit = %v, want %v", got, tc.want)
			}
		})
	}
}

func gateScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = core.AddToScheme(s)
	gvk := schema.GroupVersionKind{Group: "kubedb.com", Version: "v1", Kind: "Postgres"}
	s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(gvk.GroupVersion().WithKind("PostgresList"), &unstructured.UnstructuredList{})
	return s
}

func gatedService() *core.Service {
	tiers := []kmapi.TypedObjectReference{{APIGroup: "kubedb.com", Kind: "Postgres", Name: "orders-pg"}}
	raw, _ := json.Marshal(tiers)
	return &core.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "orders-api",
			Namespace:   "orders",
			Labels:      map[string]string{api.CattleSetNameLabel: "orders-api"},
			Annotations: map[string]string{api.GatedDataTiersAnnotation: string(raw)},
		},
		Spec: core.ServiceSpec{
			Selector: map[string]string{"app": "orders-api", api.AdmittedSelectorKey: api.AdmittedSelectorValue},
		},
	}
}

func database(dr map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubedb.com", Version: "v1", Kind: "Postgres"})
	u.SetName("orders-pg")
	u.SetNamespace("orders")
	if dr != nil {
		_ = unstructured.SetNestedMap(u.Object, dr, "status", "disasterRecovery")
	}
	return u
}

func reconcileGate(t *testing.T, svc *core.Service, db *unstructured.Unstructured) *core.Service {
	t.Helper()
	objs := []client.Object{svc}
	if db != nil {
		objs = append(objs, db)
	}
	c := fake.NewClientBuilder().WithScheme(gateScheme()).WithObjects(objs...).Build()
	a := &Agent{Client: c}
	if _, err := a.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "orders", Name: "orders-api"},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	out := &core.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "orders", Name: "orders-api"}, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func admitted(svc *core.Service) bool {
	_, present := svc.Spec.Selector[api.AdmittedSelectorKey]
	return !present
}

func TestGateAdmitWhenPromoted(t *testing.T) {
	out := reconcileGate(t, gatedService(), database(map[string]any{"role": "Primary"}))
	if !admitted(out) {
		t.Errorf("Service should be admitted when the tier is promoted; selector=%v", out.Spec.Selector)
	}
}

func TestGateRevokeWhenDemoted(t *testing.T) {
	out := reconcileGate(t, gatedService(), database(map[string]any{"role": "Standby"}))
	if admitted(out) {
		t.Errorf("Service should stay unadmitted when the tier is demoted; selector=%v", out.Spec.Selector)
	}
}

func TestGateFailClosedWhenTierAbsent(t *testing.T) {
	out := reconcileGate(t, gatedService(), nil) // no Database in this cluster
	if admitted(out) {
		t.Errorf("Service should stay unadmitted when the tier is absent (fail closed); selector=%v", out.Spec.Selector)
	}
}

func TestGateIgnoresUngatedService(t *testing.T) {
	svc := gatedService()
	delete(svc.Annotations, api.GatedDataTiersAnnotation)
	svc.Spec.Selector = map[string]string{"app": "orders-api"} // ungated renders admitted
	out := reconcileGate(t, svc, nil)
	if !admitted(out) {
		t.Errorf("ungated Service must be left admitted; selector=%v", out.Spec.Selector)
	}
}

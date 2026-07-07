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

package v1

import (
	"reflect"
	"testing"

	apps "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestDeploymentSpecCorePin pins the first eight fields of CattleSetSpec to
// apps/v1.DeploymentSpec, field for field: name, json tag, and type must match
// one for one and in the same order. A vendor bump that renames or retypes a
// mirrored upstream field fails here instead of silently diverging.
func TestDeploymentSpecCorePin(t *testing.T) {
	const coreFields = 8

	depT := reflect.TypeOf(apps.DeploymentSpec{})
	csT := reflect.TypeOf(CattleSetSpec{})

	if depT.NumField() < coreFields {
		t.Fatalf("apps/v1 DeploymentSpec has %d fields, expected at least %d; upstream layout changed", depT.NumField(), coreFields)
	}
	if csT.NumField() < coreFields {
		t.Fatalf("CattleSetSpec has %d fields, expected at least %d (the Deployment core)", csT.NumField(), coreFields)
	}

	for i := 0; i < coreFields; i++ {
		dep := depT.Field(i)
		cs := csT.Field(i)

		if dep.Name != cs.Name {
			t.Errorf("field %d: name mismatch: DeploymentSpec %q vs CattleSetSpec %q", i, dep.Name, cs.Name)
		}
		if got, want := jsonTag(cs), jsonTag(dep); got != want {
			t.Errorf("field %d (%s): json tag mismatch: DeploymentSpec %q vs CattleSetSpec %q", i, dep.Name, want, got)
		}
		if dep.Type != cs.Type {
			t.Errorf("field %d (%s): type mismatch: DeploymentSpec %s vs CattleSetSpec %s", i, dep.Name, dep.Type, cs.Type)
		}
	}
}

// TestDeploymentStatusAggregatePin pins the DeploymentStatus-mirror aggregate
// fields of CattleSetStatus (name, json tag, type) against apps/v1
// DeploymentStatus, so tooling that reads a Deployment's status reads a
// CattleSet's. These are matched by name (the CattleSet status interleaves DC/DR
// fields), not positionally.
func TestDeploymentStatusAggregatePin(t *testing.T) {
	aggregates := []string{
		"ObservedGeneration",
		"Replicas",
		"UpdatedReplicas",
		"ReadyReplicas",
		"AvailableReplicas",
		"UnavailableReplicas",
		"CollisionCount",
	}

	depT := reflect.TypeOf(apps.DeploymentStatus{})
	csT := reflect.TypeOf(CattleSetStatus{})

	for _, name := range aggregates {
		dep, ok := depT.FieldByName(name)
		if !ok {
			t.Fatalf("apps/v1 DeploymentStatus has no field %q; upstream layout changed", name)
		}
		cs, ok := csT.FieldByName(name)
		if !ok {
			t.Errorf("CattleSetStatus is missing the mirrored aggregate field %q", name)
			continue
		}
		if got, want := jsonTag(cs), jsonTag(dep); got != want {
			t.Errorf("field %s: json tag mismatch: DeploymentStatus %q vs CattleSetStatus %q", name, want, got)
		}
		if dep.Type != cs.Type {
			t.Errorf("field %s: type mismatch: DeploymentStatus %s vs CattleSetStatus %s", name, dep.Type, cs.Type)
		}
	}
}

// TestFromDeploymentRoundTrip proves a Deployment spec pastes into a CattleSet
// unchanged: FromDeployment yields a spec-identical core and sets no extension.
func TestFromDeploymentRoundTrip(t *testing.T) {
	replicas := int32(5)
	histLimit := int32(7)
	progress := int32(120)
	maxUnavail := int32(1)

	dep := &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "orders-api",
			Namespace:   "orders",
			Labels:      map[string]string{"team": "payments"},
			Annotations: map[string]string{"note": "keep"},
		},
		Spec: apps.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "orders-api"}},
			Strategy: apps.DeploymentStrategy{
				Type: apps.RollingUpdateDeploymentStrategyType,
			},
			MinReadySeconds:         10,
			RevisionHistoryLimit:    &histLimit,
			Paused:                  true,
			ProgressDeadlineSeconds: &progress,
		},
	}
	dep.Spec.Template.Labels = map[string]string{"app": "orders-api"}
	_ = maxUnavail

	cs := FromDeployment(dep)

	if cs.Kind != ResourceKindCattleSet {
		t.Errorf("kind = %q, want %q", cs.Kind, ResourceKindCattleSet)
	}
	if cs.Name != dep.Name || cs.Namespace != dep.Namespace {
		t.Errorf("identity not carried: got %s/%s", cs.Namespace, cs.Name)
	}
	if cs.Labels["team"] != "payments" || cs.Annotations["note"] != "keep" {
		t.Errorf("labels/annotations not carried: %v %v", cs.Labels, cs.Annotations)
	}

	// The reconstructed Deployment core must equal the input spec exactly.
	got := cs.DeploymentSpecCore()
	if !reflect.DeepEqual(got, dep.Spec) {
		t.Errorf("DeploymentSpecCore round-trip diverged:\n got = %#v\nwant = %#v", got, dep.Spec)
	}

	// No extension field is set.
	if cs.Spec.Distributed || cs.Spec.Topology != nil || cs.Spec.PodPlacementPolicy != nil ||
		cs.Spec.Gates != nil || cs.Spec.Traffic != nil || len(cs.Spec.SitePatches) != 0 {
		t.Errorf("FromDeployment set an extension field: %#v", cs.Spec)
	}
}

func jsonTag(f reflect.StructField) string {
	return f.Tag.Get("json")
}

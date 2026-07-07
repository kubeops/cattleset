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

	"github.com/google/go-cmp/cmp"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// deploymentCorpus is a set of Deployment specs exercising probes, strategies,
// and surge/maxUnavailable variants. The parity contract is that a CattleSet
// built from any of these via FromDeployment renders a byte-identical spec.
func deploymentCorpus() []*apps.Deployment {
	rolling := func(surge, unavail intstr.IntOrString) apps.DeploymentStrategy {
		return apps.DeploymentStrategy{
			Type: apps.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &apps.RollingUpdateDeployment{
				MaxSurge:       &surge,
				MaxUnavailable: &unavail,
			},
		}
	}
	base := func() *apps.Deployment {
		return &apps.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "orders-api", Namespace: "orders"},
			Spec: apps.DeploymentSpec{
				Replicas: ptr.To[int32](3),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "orders-api"}},
				Template: core.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "orders-api"}},
					Spec: core.PodSpec{
						Containers: []core.Container{{
							Name:  "api",
							Image: "harbor.local/orders/api:2.4.1",
							ReadinessProbe: &core.Probe{
								ProbeHandler: core.ProbeHandler{
									HTTPGet: &core.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)},
								},
							},
						}},
					},
				},
			},
		}
	}

	d1 := base()

	d2 := base()
	d2.Spec.Strategy = rolling(intstr.FromString("25%"), intstr.FromString("25%"))
	d2.Spec.MinReadySeconds = 10
	d2.Spec.RevisionHistoryLimit = ptr.To[int32](5)
	d2.Spec.ProgressDeadlineSeconds = ptr.To[int32](120)

	d3 := base()
	d3.Spec.Strategy = apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType}
	d3.Spec.Paused = true

	d4 := base()
	d4.Spec.Strategy = rolling(intstr.FromInt32(2), intstr.FromInt32(0))

	return []*apps.Deployment{d1, d2, d3, d4}
}

func TestRenderDeploymentParity(t *testing.T) {
	for i, dep := range deploymentCorpus() {
		cs := api.FromDeployment(dep)
		rendered := renderDeployment(cs)

		if diff := cmp.Diff(dep.Spec, rendered.Spec); diff != "" {
			t.Errorf("corpus[%d]: rendered Deployment spec is not byte-identical to the input core (-want +got):\n%s", i, diff)
		}
		if rendered.Name != dep.Name || rendered.Namespace != dep.Namespace {
			t.Errorf("corpus[%d]: identity not preserved: %s/%s", i, rendered.Namespace, rendered.Name)
		}
		if rendered.Labels[api.CattleSetNameLabel] != cs.Name {
			t.Errorf("corpus[%d]: identity label missing", i)
		}
	}
}

func TestProjectLocalStatus(t *testing.T) {
	cs := api.FromDeployment(deploymentCorpus()[0])
	cs.Generation = 7
	dep := &apps.Deployment{
		Status: apps.DeploymentStatus{
			Replicas:            3,
			UpdatedReplicas:     3,
			ReadyReplicas:       2,
			AvailableReplicas:   2,
			UnavailableReplicas: 1,
			Conditions: []apps.DeploymentCondition{
				{Type: apps.DeploymentAvailable, Status: core.ConditionTrue, Reason: "MinimumReplicasAvailable"},
				{Type: apps.DeploymentProgressing, Status: core.ConditionTrue, Reason: "NewReplicaSetAvailable"},
			},
		},
	}

	projectLocalStatus(cs, dep)

	if cs.Status.ReadyReplicas != 2 || cs.Status.AvailableReplicas != 2 || cs.Status.UnavailableReplicas != 1 {
		t.Errorf("aggregates not projected: %+v", cs.Status)
	}
	if cs.Status.Selector != "app=orders-api" {
		t.Errorf("selector = %q, want app=orders-api", cs.Status.Selector)
	}
	if cs.Status.ObservedGeneration != 7 {
		t.Errorf("observedGeneration = %d, want 7", cs.Status.ObservedGeneration)
	}
	if c := findCond(cs, api.ConditionAvailable); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Available condition not projected: %v", c)
	}
}

func findCond(cs *api.CattleSet, t string) *metav1.Condition {
	for i := range cs.Status.Conditions {
		if cs.Status.Conditions[i].Type == t {
			return &cs.Status.Conditions[i]
		}
	}
	return nil
}

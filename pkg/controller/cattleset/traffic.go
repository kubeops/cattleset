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
	api "kubeops.dev/cattleset/apis/apps/v1"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Traffic objects bind locally in every site and never need repointing at
// failover. The GSLB primary tag follows the holder, but that is a
// client-experience action ordered after promotion, never a fence: an
// unadmitted site's Service is endpointless regardless of what DNS says.

// renderHTTPRoute renders the per site HTTPRoute: parentRefs as given (the same
// Gateway name in every cluster), hostnames defaulting to the GSLB host, and the
// backend the local Service. It is identical in every site and unchanged by a
// failover (the no-repoint property).
func renderHTTPRoute(cs *api.CattleSet) *gwv1.HTTPRoute {
	if cs.Spec.Traffic == nil || cs.Spec.Traffic.HTTPRoute == nil || cs.Spec.Traffic.ServiceName == "" {
		return nil
	}
	hr := cs.Spec.Traffic.HTTPRoute

	hostnames := make([]gwv1.Hostname, 0, len(hr.Hostnames))
	for _, h := range hr.Hostnames {
		hostnames = append(hostnames, gwv1.Hostname(h))
	}
	if len(hostnames) == 0 && cs.Spec.Traffic.GSLB != nil && cs.Spec.Traffic.GSLB.Host != "" {
		hostnames = []gwv1.Hostname{gwv1.Hostname(cs.Spec.Traffic.GSLB.Host)}
	}

	port := gwv1.PortNumber(80)
	svcKind := gwv1.Kind("Service")
	return &gwv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: gwv1.GroupVersion.String(), Kind: "HTTPRoute"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cs.Spec.Traffic.ServiceName,
			Namespace: cs.Namespace,
			Labels:    renderedLabels(cs, nil),
		},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: hr.ParentRefs},
			Hostnames:       hostnames,
			Rules: []gwv1.HTTPRouteRule{{
				BackendRefs: []gwv1.HTTPBackendRef{{
					BackendRef: gwv1.BackendRef{
						BackendObjectReference: gwv1.BackendObjectReference{
							Kind: &svcKind,
							Name: gwv1.ObjectName(cs.Spec.Traffic.ServiceName),
							Port: &port,
						},
					},
				}},
			}},
		},
	}
}

// renderServiceExport renders a KubeSlice ServiceExport (unstructured, no
// kubeslice module dependency) so other slice members reach the herd by one
// name. The slice comes from the PlacementPolicy.
func renderServiceExport(cs *api.CattleSet, pp *petsetapi.PlacementPolicy) *unstructured.Unstructured {
	if cs.Spec.Traffic == nil || !cs.Spec.Traffic.Export || cs.Spec.Traffic.ServiceName == "" {
		return nil
	}
	if pp == nil || pp.Spec.ClusterSpreadConstraint == nil {
		return nil
	}
	slice := pp.Spec.ClusterSpreadConstraint.Slice.SliceName

	u := &unstructured.Unstructured{}
	u.SetAPIVersion("networking.kubeslice.io/v1beta1")
	u.SetKind("ServiceExport")
	u.SetName(cs.Spec.Traffic.ServiceName)
	u.SetNamespace(cs.Namespace)
	u.SetLabels(renderedLabels(cs, nil))
	_ = unstructured.SetNestedField(u.Object, slice, "spec", "slice")
	_ = unstructured.SetNestedMap(u.Object, map[string]any{
		"matchLabels": toAnyMap(cs.Spec.Selector),
	}, "spec", "selector")
	_ = unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{"name": "http", "containerPort": int64(8080), "protocol": "TCP"},
	}, "spec", "ports")
	return u
}

// renderGslb renders the k8gb Gslb (unstructured, no k8gb module dependency) per
// site: strategy failover with primaryGeoTag following the Lease holder for
// ActivePassive, or roundRobin for ActiveActive. Retagging on a holder change is
// a hub-side render update, ordered after promotion.
func renderGslb(cs *api.CattleSet, mode api.CattleMode, holder string) *unstructured.Unstructured {
	if cs.Spec.Traffic == nil || cs.Spec.Traffic.GSLB == nil || cs.Spec.Traffic.GSLB.Host == "" {
		return nil
	}
	g := cs.Spec.Traffic.GSLB

	strategy := map[string]any{}
	if mode == api.CattleModeActivePassive {
		strategy["type"] = "failover"
		strategy["primaryGeoTag"] = holder
	} else {
		strategy["type"] = "roundRobin"
	}
	if g.TTLSeconds != nil {
		strategy["dnsTtlSeconds"] = int64(*g.TTLSeconds)
	}

	u := &unstructured.Unstructured{}
	u.SetAPIVersion("k8gb.absa.oss/v1beta1")
	u.SetKind("Gslb")
	u.SetName(cs.Spec.Traffic.ServiceName)
	u.SetNamespace(cs.Namespace)
	u.SetLabels(renderedLabels(cs, nil))
	_ = unstructured.SetNestedMap(u.Object, strategy, "spec", "strategy")
	// The Gslb references the same host and the local Ingress/Service by name;
	// the resolvedGeoTags follow from the healthy endpoints, which the gate
	// controls.
	_ = unstructured.SetNestedField(u.Object, g.Host, "spec", "ingress", "rules", "host")
	return u
}

func toAnyMap(sel *metav1.LabelSelector) map[string]any {
	out := map[string]any{}
	if sel == nil {
		return out
	}
	for k, v := range sel.MatchLabels {
		out[k] = v
	}
	return out
}

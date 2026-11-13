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
	"fmt"
	"strings"
	"time"

	api "kubeops.dev/cattleset/apis/apps/v1"
	"kubeops.dev/cattleset/pkg/controller/gate"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// resolvedRefs carries the cluster reads the validator needs. Keeping it a value
// the caller supplies lets the rules run as a pure function in tests, with the
// webhook filling it from the live client. Everything here may be absent: an
// object can be applied before its PlacementPolicy or FailoverGroup exists, and a
// rule that needs a missing reference is skipped, then enforced by the controller
// (which closes the gate), rather than rejecting.
type resolvedRefs struct {
	// pp is the referenced PlacementPolicy, or nil when none is referenced or it
	// cannot be resolved.
	pp *petsetapi.PlacementPolicy

	// group is the FailoverGroup the policy references, nil when unresolved.
	group *petsetapi.FailoverGroup

	// depMembers are the members of the groups the herd's group dependsOn, when
	// every one of them resolved. Nil means unknown, not empty.
	depMembers []petsetapi.FailoverGroupMember
}

// memberSet returns the set of Member cluster names in the PlacementPolicy's
// clusterSpreadConstraint. A Witness rule is treated exactly as Arbiter (a herd
// holds no data), so only Member clusters ever appear here.
func memberSet(pp *petsetapi.PlacementPolicy) map[string]bool {
	out := map[string]bool{}
	if pp == nil || pp.Spec.ClusterSpreadConstraint == nil {
		return out
	}
	for _, r := range pp.Spec.ClusterSpreadConstraint.DistributionRules {
		if r.Role == "" || r.Role == petsetapi.DCRoleMember {
			out[r.ClusterName] = true
		}
	}
	return out
}

// clusterSet returns every cluster named in the clusterSpreadConstraint,
// regardless of role.
func clusterSet(pp *petsetapi.PlacementPolicy) map[string]bool {
	out := map[string]bool{}
	if pp == nil || pp.Spec.ClusterSpreadConstraint == nil {
		return out
	}
	for _, r := range pp.Spec.ClusterSpreadConstraint.DistributionRules {
		out[r.ClusterName] = true
	}
	return out
}

// failoverGroupRefOf is the FailoverGroup name a PlacementPolicy references.
func failoverGroupRefOf(pp *petsetapi.PlacementPolicy) string {
	if pp == nil || pp.Spec.ClusterSpreadConstraint == nil || pp.Spec.ClusterSpreadConstraint.FailoverPolicy == nil {
		return ""
	}
	if ref := pp.Spec.ClusterSpreadConstraint.FailoverPolicy.FailoverGroupRef; ref != nil {
		return ref.Name
	}
	return ""
}

// effectiveMode is the mode a herd asked for: Auto when unset.
func effectiveMode(cs *api.CattleSet) api.CattleMode { return cs.Mode() }

// ValidateCattleSet runs every create-time rule that does not compare against a
// prior object. It is pure: all cluster state arrives in refs.
func ValidateCattleSet(cs *api.CattleSet, refs resolvedRefs) field.ErrorList {
	var errs field.ErrorList
	spec := field.NewPath("spec")

	// Stateless template: reject PVC volume sources. No volumeClaimTemplates
	// field exists.
	for i, v := range cs.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			errs = append(errs, field.Invalid(
				spec.Child("template", "spec", "volumes").Index(i),
				v.Name,
				"a CattleSet template must be stateless: PersistentVolumeClaim volume sources are not allowed (use PetSet or a KubeDB Database for stateful workloads)",
			))
		}
	}

	errs = append(errs, validateRemoved(cs, spec)...)
	errs = append(errs, validateTraffic(cs, spec)...)
	errs = append(errs, validateAdmission(cs, spec)...)
	errs = append(errs, validateSyncResources(cs, spec)...)

	if cs.Spec.Distributed {
		errs = append(errs, validateDistributed(cs, refs, spec)...)
	} else {
		errs = append(errs, validateNonDistributed(cs, spec)...)
	}
	return errs
}

// validateRemoved rejects the fields that no longer exist, naming what replaces
// them. They are still in the Go type so the rejection can say so: a structural
// schema would otherwise drop them silently.
func validateRemoved(cs *api.CattleSet, spec *field.Path) field.ErrorList {
	var errs field.ErrorList
	if cs.Spec.Gates != nil {
		errs = append(errs, field.Forbidden(spec.Child("gates"),
			"gates was removed: declare what the herd depends on with dependsOn on the FailoverGroup its placement policy references (failoverPolicy.failoverGroupRef)"))
	}
	if cs.Spec.Traffic != nil && cs.Spec.Traffic.Export {
		errs = append(errs, field.Forbidden(spec.Child("traffic", "export"),
			"traffic.export was removed: a stateless CattleSet does not use KubeSlice"))
	}
	return errs
}

// validateTraffic checks the rendered Service's shape.
func validateTraffic(cs *api.CattleSet, spec *field.Path) field.ErrorList {
	var errs field.ErrorList
	t := cs.Spec.Traffic
	if t == nil {
		return errs
	}
	p := spec.Child("traffic")
	if t.ServiceName == "" {
		errs = append(errs, field.Required(p.Child("serviceName"), "serviceName is required when traffic is set"))
	}
	if len(t.Ports) == 0 {
		errs = append(errs, field.Required(p.Child("ports"), "ports is required when traffic is set: the controller does not assume a port mapping"))
	}
	seen := map[string]bool{}
	for i, port := range t.Ports {
		pp := p.Child("ports").Index(i)
		if port.Port <= 0 || port.Port > 65535 {
			errs = append(errs, field.Invalid(pp.Child("port"), port.Port, "must be between 1 and 65535"))
		}
		if len(t.Ports) > 1 && port.Name == "" {
			errs = append(errs, field.Required(pp.Child("name"), "a port needs a name when the Service has more than one"))
		}
		if port.Name != "" {
			if seen[port.Name] {
				errs = append(errs, field.Duplicate(pp.Child("name"), port.Name))
			}
			seen[port.Name] = true
		}
	}
	// The Service selector is a plain label map: matchExpressions cannot be
	// expressed in it and would be dropped silently, serving the wrong pods.
	if cs.Spec.Selector != nil && len(cs.Spec.Selector.MatchExpressions) > 0 {
		errs = append(errs, field.Forbidden(spec.Child("selector", "matchExpressions"),
			"a CattleSet with traffic must select with matchLabels only: the Service selector cannot express matchExpressions"))
	}
	if t.Network != nil && t.Network.Type != "" && t.Network.Type != "None" {
		errs = append(errs, field.NotSupported(p.Child("network", "type"), t.Network.Type, []string{"None"}))
	}
	return errs
}

// validateAdmission bounds the gate's tuning in the safe direction only.
func validateAdmission(cs *api.CattleSet, spec *field.Path) field.ErrorList {
	var errs field.ErrorList
	a := cs.Spec.Admission
	if a == nil {
		return errs
	}
	p := spec.Child("admission")
	if a.MarkerMaxAge != nil {
		if a.MarkerMaxAge.Duration <= 0 {
			errs = append(errs, field.Invalid(p.Child("markerMaxAge"), a.MarkerMaxAge.Duration.String(), "must be positive"))
		}
		if a.MarkerMaxAge.Duration > api.DefaultMarkerMaxAge {
			errs = append(errs, field.Invalid(p.Child("markerMaxAge"), a.MarkerMaxAge.Duration.String(),
				fmt.Sprintf("must not exceed %s, the marker TTL the database fence uses: admitting an application in a DC the database already considers inactive defeats the ordering", api.DefaultMarkerMaxAge)))
		}
	}
	if a.PrimaryLossGrace != nil && a.PrimaryLossGrace.Duration < 0 {
		errs = append(errs, field.Invalid(p.Child("primaryLossGrace"), a.PrimaryLossGrace.Duration.String(), "must not be negative"))
	}
	if a.PrimaryLossGrace != nil && a.PrimaryLossGrace.Duration > time.Minute {
		errs = append(errs, field.Invalid(p.Child("primaryLossGrace"), a.PrimaryLossGrace.Duration.String(),
			"must not exceed 1m: it only has to ride out a local re-election (4 to 6 s for Postgres)"))
	}
	return errs
}

// validateSyncResources limits what the controller will copy to the spokes.
func validateSyncResources(cs *api.CattleSet, spec *field.Path) field.ErrorList {
	var errs field.ErrorList
	type key struct{ kind, name string }
	seen := map[key]bool{}
	for i, r := range cs.Spec.SyncResources {
		p := spec.Child("syncResources").Index(i)
		switch r.Kind {
		case "ConfigMap", "Secret", "ServiceAccount":
		default:
			errs = append(errs, field.NotSupported(p.Child("kind"), r.Kind, []string{"ConfigMap", "Secret", "ServiceAccount"}))
		}
		if r.APIGroup != nil && *r.APIGroup != "" {
			errs = append(errs, field.Invalid(p.Child("apiGroup"), *r.APIGroup, "only core resources can be synced"))
		}
		if r.Name == "" {
			errs = append(errs, field.Required(p.Child("name"), ""))
		}
		k := key{r.Kind, r.Name}
		if seen[k] {
			errs = append(errs, field.Duplicate(p, r.Kind+"/"+r.Name))
		}
		seen[k] = true
	}
	return errs
}

func validateNonDistributed(cs *api.CattleSet, spec *field.Path) field.ErrorList {
	var errs field.ErrorList
	// distributed:false forbids the whole DC/DR extension except a
	// PlacementPolicy reference carrying only local constraints.
	if cs.Spec.Topology != nil {
		errs = append(errs, field.Forbidden(spec.Child("topology"), "topology is only allowed when distributed is true"))
	}
	if len(cs.Spec.SitePatches) > 0 {
		errs = append(errs, field.Forbidden(spec.Child("sitePatches"), "sitePatches is only allowed when distributed is true"))
	}
	if cs.Spec.RolloutOrder != "" {
		errs = append(errs, field.Forbidden(spec.Child("rolloutOrder"), "rolloutOrder is only allowed when distributed is true"))
	}
	if cs.Spec.Admission != nil {
		errs = append(errs, field.Forbidden(spec.Child("admission"), "admission is only allowed when distributed is true"))
	}
	if len(cs.Spec.SyncResources) > 0 {
		errs = append(errs, field.Forbidden(spec.Child("syncResources"), "syncResources is only allowed when distributed is true: a non-distributed herd runs in the namespace the objects are in"))
	}
	if t := cs.Spec.Traffic; t != nil {
		if t.GSLB != nil {
			errs = append(errs, field.Forbidden(spec.Child("traffic", "gslb"), "traffic.gslb is only allowed when distributed is true"))
		}
		if t.Network != nil {
			errs = append(errs, field.Forbidden(spec.Child("traffic", "network"), "traffic.network is only allowed when distributed is true"))
		}
		if t.Signal {
			errs = append(errs, field.Forbidden(spec.Child("traffic", "signal"), "traffic.signal is only allowed when distributed is true"))
		}
	}
	return errs
}

func validateDistributed(cs *api.CattleSet, refs resolvedRefs, spec *field.Path) field.ErrorList {
	var errs field.ErrorList

	// distributed:true requires podPlacementPolicy with a clusterSpreadConstraint.
	if cs.Spec.PodPlacementPolicy == nil {
		errs = append(errs, field.Required(spec.Child("podPlacementPolicy"), "distributed CattleSet requires a podPlacementPolicy with a clusterSpreadConstraint"))
	} else if refs.pp != nil && refs.pp.Spec.ClusterSpreadConstraint == nil {
		errs = append(errs, field.Invalid(spec.Child("podPlacementPolicy"), cs.Spec.PodPlacementPolicy.Name,
			"the referenced PlacementPolicy has no clusterSpreadConstraint (required for a distributed CattleSet)"))
	}

	mode := effectiveMode(cs)
	members := memberSet(refs.pp)
	clusters := clusterSet(refs.pp)

	// Posture-specific PlacementPolicy requirements (only when resolvable).
	if refs.pp != nil && refs.pp.Spec.ClusterSpreadConstraint != nil {
		csc := refs.pp.Spec.ClusterSpreadConstraint
		group := failoverGroupRefOf(refs.pp)
		switch mode {
		case api.CattleModeActivePassive:
			if csc.FailoverPolicy == nil {
				errs = append(errs, field.Invalid(spec.Child("topology", "mode"), mode,
					"ActivePassive requires the referenced PlacementPolicy to carry a failoverPolicy (a FailoverGroup to follow)"))
			}
		case api.CattleModeActiveActive:
			if len(members) < 2 {
				errs = append(errs, field.Invalid(spec.Child("topology", "mode"), mode,
					"ActiveActive requires at least two Member clusters in the referenced PlacementPolicy"))
			}
		}
		// Anything that is not an explicit ActiveActive follows a FailoverGroup: it is
		// the only source of the herd's Lease and of its ordering.
		if mode != api.CattleModeActiveActive && group == "" {
			errs = append(errs, field.Required(spec.Child("podPlacementPolicy"),
				"the referenced PlacementPolicy needs failoverPolicy.failoverGroupRef: a herd that follows a data tier must be a member of a FailoverGroup that dependsOn it (set topology.mode to ActiveActive for a herd with no data dependency)"))
		}
	}

	// An explicit ActiveActive cannot sit over a database whose standby DC has no
	// writable endpoint: the application reaches the database only through its local
	// Service. Only checkable when the dependencies resolve; otherwise the
	// controller reports it and fails closed.
	if mode == api.CattleModeActiveActive {
		for _, m := range refs.depMembers {
			if c := gate.CampFor(m.APIGroup, m.Kind); c != gate.CampLeaderlessGeoNative {
				errs = append(errs, field.Invalid(spec.Child("topology", "mode"), mode,
					fmt.Sprintf("ActiveActive over a %s dependency (%s %s/%s) is not supported: the application reaches the database only through its local Service, which has no endpoints in a standby DC; use Auto or ActivePassive", c, m.Kind, m.Namespace, m.Name)))
				break
			}
		}
	}

	// distribution may name only Member clusters present in the distributionRules.
	if cs.Spec.Topology != nil {
		for i, share := range cs.Spec.Topology.Distribution {
			p := spec.Child("topology", "distribution").Index(i).Child("clusterName")
			if refs.pp == nil {
				continue
			}
			if !clusters[share.ClusterName] {
				errs = append(errs, field.Invalid(p, share.ClusterName, "not a cluster in the referenced PlacementPolicy's distributionRules"))
			} else if !members[share.ClusterName] {
				errs = append(errs, field.Invalid(p, share.ClusterName, "distribution may weight only Member clusters"))
			}
		}
	}

	// sitePatches may name only clusters in the distributionRules, and may not
	// touch replicas, selector, the selector labels, or the admission gate.
	for i, sp := range cs.Spec.SitePatches {
		p := spec.Child("sitePatches").Index(i)
		if refs.pp != nil && !clusters[sp.ClusterName] {
			errs = append(errs, field.Invalid(p.Child("clusterName"), sp.ClusterName, "not a cluster in the referenced PlacementPolicy's distributionRules"))
		}
		errs = append(errs, validateSitePatchScope(cs, sp, p.Child("patch"))...)
	}
	return errs
}

// WarnCattleSet returns the advisories that should not block an object: it may be
// applied before what it refers to exists, and some users deliver pod
// dependencies by GitOps.
func WarnCattleSet(cs *api.CattleSet, refs resolvedRefs, arbiterHub bool) []string {
	var warns []string
	if !cs.Spec.Distributed {
		if arbiterHub {
			warns = append(warns, "this cluster is an Arbiter DC and runs no workloads; a non-distributed CattleSet created here runs nowhere else. Set distributed: true, or create it on a Member cluster")
		}
		return warns
	}
	if refs.pp != nil && failoverGroupRefOf(refs.pp) != "" && refs.group == nil {
		warns = append(warns, fmt.Sprintf("FailoverGroup %q does not exist yet: the Service stays closed (unadmitted) until it does and its dependencies have reported", failoverGroupRefOf(refs.pp)))
	}
	listed := map[string]bool{}
	for _, r := range cs.Spec.SyncResources {
		listed[r.Kind+"/"+r.Name] = true
	}
	for _, ref := range podReferences(cs) {
		if !listed[ref] {
			warns = append(warns, ref+" is referenced by the pod template but not listed in syncResources: it must already exist on every Member cluster, or the pods will not start there")
		}
	}
	return warns
}

// podReferences lists the ConfigMaps, Secrets and ServiceAccounts the pod
// template refers to by name, as "Kind/name".
func podReferences(cs *api.CattleSet) []string {
	pod := cs.Spec.Template.Spec
	set := map[string]bool{}
	add := func(kind, name string) {
		if name != "" {
			set[kind+"/"+name] = true
		}
	}
	if pod.ServiceAccountName != "" && pod.ServiceAccountName != "default" {
		add("ServiceAccount", pod.ServiceAccountName)
	}
	for _, s := range pod.ImagePullSecrets {
		add("Secret", s.Name)
	}
	for _, v := range pod.Volumes {
		switch {
		case v.ConfigMap != nil:
			add("ConfigMap", v.ConfigMap.Name)
		case v.Secret != nil:
			add("Secret", v.Secret.SecretName)
		case v.Projected != nil:
			for _, src := range v.Projected.Sources {
				if src.ConfigMap != nil {
					add("ConfigMap", src.ConfigMap.Name)
				}
				if src.Secret != nil {
					add("Secret", src.Secret.Name)
				}
			}
		}
	}
	containers := append(append([]core.Container{}, pod.InitContainers...), pod.Containers...)
	for _, c := range containers {
		for _, e := range c.EnvFrom {
			if e.ConfigMapRef != nil {
				add("ConfigMap", e.ConfigMapRef.Name)
			}
			if e.SecretRef != nil {
				add("Secret", e.SecretRef.Name)
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				continue
			}
			if e.ValueFrom.ConfigMapKeyRef != nil {
				add("ConfigMap", e.ValueFrom.ConfigMapKeyRef.Name)
			}
			if e.ValueFrom.SecretKeyRef != nil {
				add("Secret", e.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && strings.Compare(s[j-1], s[j]) > 0; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// validateSitePatchScope rejects a strategic merge patch that reaches into
// replicas, selector, the labels the selector matches, or the admission gate.
func validateSitePatchScope(cs *api.CattleSet, sp api.SitePatch, p *field.Path) field.ErrorList {
	var errs field.ErrorList
	if len(sp.Patch.Raw) == 0 {
		return errs
	}
	patch, ok := decodeSpecPatch(sp.Patch.Raw)
	if !ok {
		return errs // a malformed patch is caught by the strategic merge at render time
	}
	if _, present := patch["replicas"]; present {
		errs = append(errs, field.Forbidden(p.Child("spec", "replicas"), "a sitePatch may not touch replicas (the controller is the single writer of per-site replica counts)"))
	}
	if _, present := patch["selector"]; present {
		errs = append(errs, field.Forbidden(p.Child("spec", "selector"), "a sitePatch may not touch the selector"))
	}
	// selector labels and the admission gate live under template.metadata.labels.
	selLabels := map[string]bool{}
	if cs.Spec.Selector != nil {
		for k := range cs.Spec.Selector.MatchLabels {
			selLabels[k] = true
		}
	}
	if tmplLabels := patchedTemplateLabels(patch); tmplLabels != nil {
		for k := range tmplLabels {
			if selLabels[k] {
				errs = append(errs, field.Forbidden(p.Child("spec", "template", "metadata", "labels", k), "a sitePatch may not touch a label the selector matches"))
			}
			if k == api.AdmittedSelectorKey {
				errs = append(errs, field.Forbidden(p.Child("spec", "template", "metadata", "labels", k), "a sitePatch may not touch the admission gate label"))
			}
		}
	}
	return errs
}

// ValidateCattleSetUpdate enforces the update rules: the selector is immutable,
// and flipping distributed true to false is rejected. topology.mode is mutable:
// Auto makes the mode a policy, not an identity, and changing it never recreates
// objects (the controller re-derives it, and only a spec change may relax it).
func ValidateCattleSetUpdate(oldCS, newCS *api.CattleSet) field.ErrorList {
	var errs field.ErrorList
	spec := field.NewPath("spec")

	if !selectorEqual(oldCS.Spec.Selector, newCS.Spec.Selector) {
		errs = append(errs, field.Forbidden(spec.Child("selector"), "selector is immutable"))
	}
	if oldCS.Spec.Distributed && !newCS.Spec.Distributed {
		errs = append(errs, field.Forbidden(spec.Child("distributed"), "disabling distribution (true to false) is rejected in v1"))
	}
	return errs
}

func selectorEqual(a, b *metav1.LabelSelector) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if len(a.MatchLabels) != len(b.MatchLabels) || len(a.MatchExpressions) != len(b.MatchExpressions) {
		return false
	}
	for k, v := range a.MatchLabels {
		if b.MatchLabels[k] != v {
			return false
		}
	}
	// MatchExpressions comparison kept simple: length plus per-index key/op/values.
	for i := range a.MatchExpressions {
		ax, bx := a.MatchExpressions[i], b.MatchExpressions[i]
		if ax.Key != bx.Key || ax.Operator != bx.Operator || len(ax.Values) != len(bx.Values) {
			return false
		}
		for j := range ax.Values {
			if ax.Values[j] != bx.Values[j] {
				return false
			}
		}
	}
	return true
}

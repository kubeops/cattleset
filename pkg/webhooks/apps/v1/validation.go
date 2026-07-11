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

	api "kubeops.dev/cattleset/apis/apps/v1"
	petsetapi "kubeops.dev/petset/apis/apps/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// resolvedRefs carries the cluster reads the validator needs. Keeping it a
// value the caller supplies lets the rules run as a pure function in tests,
// with the webhook filling it from the live client.
type resolvedRefs struct {
	// pp is the referenced PlacementPolicy, or nil when none is referenced or
	// it cannot be resolved (a dangling reference is not itself rejected: the
	// object may be applied before its PlacementPolicy).
	pp *petsetapi.PlacementPolicy

	// dataTierTriggers holds one entry per gates.dataTiers reference, in order,
	// each the trigger that data tier follows (nil when unresolved). It drives
	// the same-trigger rule. Nil (the whole slice) skips the live check.
	dataTierTriggers []*petsetapi.FailoverTrigger
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

// ValidateCattleSet runs every create-time rule from design Section 8 that does
// not compare against a prior object. It is pure: all cluster state arrives in
// refs.
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

	if cs.Spec.Distributed {
		errs = append(errs, validateDistributed(cs, refs, spec)...)
	} else {
		errs = append(errs, validateNonDistributed(cs, spec)...)
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
	if cs.Spec.Gates != nil {
		errs = append(errs, field.Forbidden(spec.Child("gates"), "gates is only allowed when distributed is true"))
	}
	if cs.Spec.Traffic != nil {
		if cs.Spec.Traffic.Export {
			errs = append(errs, field.Forbidden(spec.Child("traffic", "export"), "traffic.export is only allowed when distributed is true"))
		}
		if cs.Spec.Traffic.GSLB != nil {
			errs = append(errs, field.Forbidden(spec.Child("traffic", "gslb"), "traffic.gslb is only allowed when distributed is true"))
		}
	}
	return errs
}

func validateDistributed(cs *api.CattleSet, refs resolvedRefs, spec *field.Path) field.ErrorList {
	var errs field.ErrorList

	// distributed:true requires podPlacementPolicy (with a
	// clusterSpreadConstraint) and topology.
	if cs.Spec.PodPlacementPolicy == nil {
		errs = append(errs, field.Required(spec.Child("podPlacementPolicy"), "distributed CattleSet requires a podPlacementPolicy with a clusterSpreadConstraint"))
	} else if refs.pp != nil && refs.pp.Spec.ClusterSpreadConstraint == nil {
		errs = append(errs, field.Invalid(spec.Child("podPlacementPolicy"), cs.Spec.PodPlacementPolicy.Name,
			"the referenced PlacementPolicy has no clusterSpreadConstraint (required for a distributed CattleSet)"))
	}
	if cs.Spec.Topology == nil {
		errs = append(errs, field.Required(spec.Child("topology"), "distributed CattleSet requires a topology"))
		return errs
	}

	mode := cs.Spec.Topology.Mode
	members := memberSet(refs.pp)
	clusters := clusterSet(refs.pp)

	// Posture-specific PlacementPolicy requirements (only when resolvable).
	if refs.pp != nil && refs.pp.Spec.ClusterSpreadConstraint != nil {
		csc := refs.pp.Spec.ClusterSpreadConstraint
		switch mode {
		case api.CattleModeActivePassive:
			if csc.FailoverPolicy == nil {
				errs = append(errs, field.Invalid(spec.Child("topology", "mode"), mode,
					"ActivePassive requires the referenced PlacementPolicy to carry a failoverPolicy (a trigger to follow)"))
			}
		case api.CattleModeActiveActive:
			if len(members) < 2 {
				errs = append(errs, field.Invalid(spec.Child("topology", "mode"), mode,
					"ActiveActive requires at least two Member clusters in the referenced PlacementPolicy"))
			}
		}
	}

	// distribution may name only Member clusters present in the distributionRules.
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

	// sitePatches may name only clusters in the distributionRules, and may not
	// touch replicas, selector, the selector labels, or the admission gate.
	for i, sp := range cs.Spec.SitePatches {
		p := spec.Child("sitePatches").Index(i)
		if refs.pp != nil && !clusters[sp.ClusterName] {
			errs = append(errs, field.Invalid(p.Child("clusterName"), sp.ClusterName, "not a cluster in the referenced PlacementPolicy's distributionRules"))
		}
		errs = append(errs, validateSitePatchScope(cs, sp, p.Child("patch"))...)
	}

	// Gates require ActivePassive and same-trigger data tiers.
	if cs.Spec.Gates != nil {
		errs = append(errs, validateGates(cs, refs, spec.Child("gates"))...)
	}

	return errs
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

func validateGates(cs *api.CattleSet, refs resolvedRefs, p *field.Path) field.ErrorList {
	var errs field.ErrorList

	// gates requires topology.mode ActivePassive: write-path gating implies a
	// single writer, and a gated-shut ActiveActive standby is ActivePassive in
	// disguise.
	if cs.Spec.Topology == nil || cs.Spec.Topology.Mode != api.CattleModeActivePassive {
		errs = append(errs, field.Forbidden(p, "gates requires topology.mode ActivePassive"))
	}
	if len(cs.Spec.Gates.DataTiers) == 0 {
		errs = append(errs, field.Required(p.Child("dataTiers"), "gates requires at least one data tier"))
		return errs
	}

	// Every gated data tier must follow the SAME failoverPolicy trigger as the
	// herd. Fail over together or gate forever. Checked only when both the
	// herd's trigger and the tiers' triggers are resolvable.
	herdTrigger := herdTrigger(refs.pp)
	if herdTrigger == nil || refs.dataTierTriggers == nil {
		return errs
	}
	for i, t := range refs.dataTierTriggers {
		if t == nil {
			continue // unresolved tier; the live webhook reports absence separately
		}
		if !sameTrigger(*herdTrigger, *t) {
			errs = append(errs, field.Invalid(p.Child("dataTiers").Index(i), cs.Spec.Gates.DataTiers[i].Name,
				fmt.Sprintf("data tier follows a different failover trigger (%s) than the herd (%s); gated tiers must fail over together", triggerString(*t), triggerString(*herdTrigger))))
		}
	}
	return errs
}

func herdTrigger(pp *petsetapi.PlacementPolicy) *petsetapi.FailoverTrigger {
	if pp == nil || pp.Spec.ClusterSpreadConstraint == nil || pp.Spec.ClusterSpreadConstraint.FailoverPolicy == nil {
		return nil
	}
	return &pp.Spec.ClusterSpreadConstraint.FailoverPolicy.Trigger
}

func sameTrigger(a, b petsetapi.FailoverTrigger) bool {
	return a.Scope == b.Scope && a.Group == b.Group
}

func triggerString(t petsetapi.FailoverTrigger) string {
	if t.Scope == petsetapi.FailoverScopeGroup {
		return fmt.Sprintf("Group/%s", t.Group)
	}
	return string(t.Scope)
}

// ValidateCattleSetUpdate enforces the v1 immutability rules: selector and
// topology.mode are immutable, and flipping distributed true to false is
// rejected.
func ValidateCattleSetUpdate(oldCS, newCS *api.CattleSet) field.ErrorList {
	var errs field.ErrorList
	spec := field.NewPath("spec")

	if !selectorEqual(oldCS.Spec.Selector, newCS.Spec.Selector) {
		errs = append(errs, field.Forbidden(spec.Child("selector"), "selector is immutable"))
	}

	oldMode, newMode := modeOf(oldCS), modeOf(newCS)
	if oldMode != "" && newMode != "" && oldMode != newMode {
		errs = append(errs, field.Forbidden(spec.Child("topology", "mode"), "topology.mode is immutable"))
	}

	if oldCS.Spec.Distributed && !newCS.Spec.Distributed {
		errs = append(errs, field.Forbidden(spec.Child("distributed"), "disabling distribution (true to false) is rejected in v1"))
	}

	return errs
}

func modeOf(cs *api.CattleSet) api.CattleMode {
	if cs.Spec.Topology == nil {
		return ""
	}
	return cs.Spec.Topology.Mode
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

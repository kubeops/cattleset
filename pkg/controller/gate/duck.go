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
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// tierState is the local view of one gated data tier: whether the object exists
// in this cluster and whether it reports this site as its promoted, writable
// primary.
type tierState struct {
	Name     string
	Present  bool
	Promoted bool
}

// isPromotedHere duck-types a data tier's status.disasterRecovery to decide
// whether the local site is the promoted, writable primary, without a module
// dependency on kubedb. It reads whichever of the recognized signals is present
// (a promoted boolean, or a role/mode/status naming the primary/active state)
// and fails closed: an absent or unrecognized status is not promoted.
func isPromotedHere(u *unstructured.Unstructured) bool {
	dr, found, err := unstructured.NestedMap(u.Object, "status", "disasterRecovery")
	if err != nil || !found || dr == nil {
		return false
	}

	if b, ok := dr["promoted"].(bool); ok {
		return b
	}
	for _, key := range []string{"role", "mode", "status", "phase"} {
		if s, ok := dr[key].(string); ok && isPrimaryWord(s) {
			return true
		}
	}
	return false
}

func isPrimaryWord(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "primary", "active", "promoted", "leader", "writable":
		return true
	}
	return false
}

// decideAdmit returns true only when every gated tier is present and promoted
// here. Any tier demoted, unknown, or absent revokes admission (fail closed).
func decideAdmit(states []tierState) bool {
	if len(states) == 0 {
		return false
	}
	for _, s := range states {
		if !s.Present || !s.Promoted {
			return false
		}
	}
	return true
}

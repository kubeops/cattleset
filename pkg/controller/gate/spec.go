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
	"encoding/json"
	"strings"
)

// SpecVersion is the only gate annotation schema version this agent understands.
// An unknown version is treated as Closed, never as Open.
const SpecVersion = 1

// GateMode says how a site decides to admit traffic.
type GateMode string

const (
	// GateModeTiers admits only while every tier admits (AND).
	GateModeTiers GateMode = "Tiers"
	// GateModeAppMarker admits only while the herd's own scope marker is fresh and
	// names this DC. Used by an ActivePassive herd whose group has no dependencies.
	GateModeAppMarker GateMode = "AppMarker"
	// GateModeOpen always admits and reads no marker. The hub renders it as a
	// Service with no admission term at all, so the agent never sees it; it is
	// listed so the contract is complete.
	GateModeOpen GateMode = "Open"
	// GateModeClosed never admits. The hub renders it whenever dependencies are
	// unresolved, so "nothing to check" can never be read as "open".
	GateModeClosed GateMode = "Closed"
)

// Camp is what "active" means for a data tier engine. Only Postgres is documented
// and tested; every other row is derived from the DC-DR matrix and unvalidated,
// so the agent treats everything but LeaderlessGeoNative restrictively.
type Camp string

const (
	CampSingleWriter        Camp = "SingleWriter"
	CampAsyncMirror         Camp = "AsyncMirror"
	CampQuorumNative        Camp = "QuorumNative"
	CampLeaderlessGeoNative Camp = "LeaderlessGeoNative"
)

// GateSpec is the document the hub controller stores in the Service annotation
// apps.k8s.appscode.com/gate. The agent has no hub client, so this and the local
// objects are everything it knows.
type GateSpec struct {
	Version int      `json:"version"`
	Mode    GateMode `json:"mode"`

	// AppLease is the herd's own Lease (and marker) name, used by AppMarker.
	AppLease string `json:"appLease,omitempty"`

	// MarkerMaxAgeSeconds is the marker freshness window. Zero or anything above
	// DefaultMarkerMaxAgeSeconds means the default: the window may only shrink.
	MarkerMaxAgeSeconds int32 `json:"markerMaxAgeSeconds,omitempty"`

	// PrimaryLossGraceSeconds is how long "no writable primary here" alone is
	// ridden out while the marker still says active. Zero disables the grace, so
	// a missing field is the safe direction.
	PrimaryLossGraceSeconds int32 `json:"primaryLossGraceSeconds"`

	// DrainPodLabel makes the agent label the herd's pods serving=true|false.
	DrainPodLabel bool `json:"drainPodLabel,omitempty"`

	Tiers []GateTier `json:"tiers,omitempty"`
}

// DefaultMarkerMaxAgeSeconds is pg-coordinator's marker TTL.
const DefaultMarkerMaxAgeSeconds = 30

// GateTier is one data tier the herd depends on.
type GateTier struct {
	// Lease is the dependency group's Lease name; the marker ConfigMap in
	// dc-failover has the same name, and its break-glass override is Lease+"-override".
	Lease string `json:"lease"`

	// Namespace and Name identify the database.
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`

	// Camp is what active means for this engine.
	Camp Camp `json:"camp"`

	// Service is the database's local client Service. Check C3 is "this Service
	// has a ready endpoint".
	Service string `json:"service"`

	// InstanceSelector is the C3 fallback when the Service has no ready endpoint
	// for a reason the Service cannot show: a Ready pod matching it that carries
	// kubedb.com/role=primary.
	InstanceSelector map[string]string `json:"instanceSelector,omitempty"`
}

// ClosedSpec is what the agent uses for anything it cannot read.
func ClosedSpec() GateSpec { return GateSpec{Version: SpecVersion, Mode: GateModeClosed} }

// ParseSpec decodes the gate annotation. Absent, empty, unparsable, an unknown
// version or mode, Tiers mode with no tiers, or AppMarker mode with no lease all
// mean Closed. This is the only place the agent turns hub input into policy.
func ParseSpec(raw string, present bool) GateSpec {
	if !present || strings.TrimSpace(raw) == "" {
		return ClosedSpec()
	}
	var s GateSpec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return ClosedSpec()
	}
	if s.Version != SpecVersion {
		return ClosedSpec()
	}
	switch s.Mode {
	case GateModeTiers:
		if len(s.Tiers) == 0 {
			return ClosedSpec()
		}
		for _, t := range s.Tiers {
			if t.Lease == "" || t.Service == "" {
				return ClosedSpec()
			}
		}
	case GateModeAppMarker:
		if s.AppLease == "" {
			return ClosedSpec()
		}
	case GateModeOpen, GateModeClosed:
	default:
		return ClosedSpec()
	}
	return s
}

// Encode renders the spec as the annotation value.
func (s GateSpec) Encode() (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

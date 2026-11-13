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
	"time"
)

// Admission reasons. They are a bounded set so they can be a metric label and a
// status field.
const (
	ReasonAdmitted      = "Admitted"
	ReasonMarkerMissing = "MarkerMissing"
	ReasonMarkerStale   = "MarkerStale"
	ReasonNotActiveHere = "NotActiveHere"
	ReasonQuiescing     = "Quiescing"
	ReasonNoPrimaryHere = "NoPrimaryHere"
	ReasonNoInstance    = "NoInstanceHere"
	ReasonClosed        = "Closed"
)

// maxClockSkew is how far in the future a marker's renewTime may be before it is
// distrusted. renewTime is stamped in another DC and compared with this spoke's
// clock (NTP across DCs is a requirement, as for pg-coordinator); a renewTime
// well ahead of local time would otherwise look fresh for as long as it is ahead.
const maxClockSkew = 10 * time.Second

// Marker is the spoke-local scope marker ConfigMap dc-failover/<lease>, projected
// by the DC's dr-controlplane agent.
type Marker struct {
	Present  bool
	ActiveDC string
	Renew    time.Time
	Quiesce  string
}

// TierObservation is everything the agent reads locally about one tier.
type TierObservation struct {
	Marker Marker
	// OverrideActive is true when the break-glass ConfigMap <lease>-override
	// exists on this spoke. Presence is the whole signal (dr-controlplane and
	// pg-coordinator ignore its data), and the spoke is the DC it applies to.
	OverrideActive bool
	// PrimaryReady means the local database Service has a ready endpoint (or the
	// fallback pod check passed).
	PrimaryReady bool
	// AnyInstanceReady is for camps with no primary label: a ready instance exists.
	AnyInstanceReady bool
}

// AdmitTier says whether this spoke may admit traffic for one tier, and a reason
// when not. Order matters: the cheapest, most fail-closed checks come first.
//
//	C1 active here: marker names this DC and is fresh        (or C4)
//	C2 not quiescing: no planned switchover has frozen writes
//	C3 writable here: the local Service has a ready endpoint
//	C4 break-glass: <lease>-override on this spoke names this DC
//
// LeaderlessGeoNative needs only a local ready instance. Every other camp,
// including the unvalidated AsyncMirror and QuorumNative, gets the full set: when
// an engine's row is unproven the restrictive reading is the safe one.
func AdmitTier(t GateTier, o TierObservation, thisDC string, maxAge time.Duration, now time.Time) (bool, string) {
	if t.Camp == CampLeaderlessGeoNative {
		if !o.AnyInstanceReady {
			return false, ReasonNoInstance
		}
		return true, ReasonAdmitted
	}

	switch {
	case o.OverrideActive:
		// C4: the operator forced this DC writable without the hub.
	case !o.Marker.Present:
		return false, ReasonMarkerMissing
	case !markerFresh(o.Marker, maxAge, now):
		return false, ReasonMarkerStale
	case o.Marker.ActiveDC != thisDC:
		return false, ReasonNotActiveHere
	}
	if o.Marker.Present && o.Marker.Quiesce != "" && o.Marker.Quiesce == thisDC {
		return false, ReasonQuiescing
	}
	if !o.PrimaryReady {
		return false, ReasonNoPrimaryHere
	}
	return true, ReasonAdmitted
}

// AdmitAppMarker is the AppMarker mode: the herd's own marker must be fresh and
// name this DC, and a planned switchover must not be freezing it.
func AdmitAppMarker(o TierObservation, thisDC string, maxAge time.Duration, now time.Time) (bool, string) {
	switch {
	case o.OverrideActive:
	case !o.Marker.Present:
		return false, ReasonMarkerMissing
	case !markerFresh(o.Marker, maxAge, now):
		return false, ReasonMarkerStale
	case o.Marker.ActiveDC != thisDC:
		return false, ReasonNotActiveHere
	}
	if o.Marker.Present && o.Marker.Quiesce != "" && o.Marker.Quiesce == thisDC {
		return false, ReasonQuiescing
	}
	return true, ReasonAdmitted
}

func markerFresh(m Marker, maxAge time.Duration, now time.Time) bool {
	if m.Renew.IsZero() {
		return false // unparsable or absent renewTime
	}
	age := now.Sub(m.Renew)
	if age < -maxClockSkew {
		return false // from the future: distrust it
	}
	return age <= maxAge
}

// EffectiveMaxAge turns the spec's window into a duration: never above the
// 30 s the database fence uses, and the default when unset.
func EffectiveMaxAge(s GateSpec) time.Duration {
	secs := s.MarkerMaxAgeSeconds
	if secs <= 0 || secs > DefaultMarkerMaxAgeSeconds {
		secs = DefaultMarkerMaxAgeSeconds
	}
	return time.Duration(secs) * time.Second
}

// Decision is the outcome of evaluating a whole gate.
type Decision struct {
	Admit  bool
	Reason string
	// Holding is true when the answer is Admit only because the primary-loss
	// grace is still running. The agent re-evaluates at RecheckIn.
	Holding bool
	// RecheckIn, when non-zero, is when the grace ends and the decision must be
	// taken again even if no watched object changed.
	RecheckIn time.Duration
}

// Grace is the per Service state the primary-loss hysteresis needs. The zero
// value means "no loss in progress". It lives in agent memory: an agent restart
// starts the grace over, which can only delay a revoke by one grace window.
type Grace struct {
	since time.Time
}

// Evaluate decides admission for a whole gate. obs[i] is the observation for
// spec.Tiers[i]; app is the observation for AppMarker mode. wasAdmitted is whether
// the Service currently selects endpoints.
//
// Hysteresis (design 5.4): only "no writable primary here" while every marker
// check still passes is graced, and only while the site was admitted. Marker
// loss, another DC named, and quiesce revoke at once. Any recovery admits at once.
func Evaluate(spec GateSpec, obs []TierObservation, app TierObservation, thisDC string, wasAdmitted bool, g *Grace, now time.Time) Decision {
	maxAge := EffectiveMaxAge(spec)
	grace := time.Duration(spec.PrimaryLossGraceSeconds) * time.Second
	if grace < 0 {
		grace = 0
	}

	var reasons []string
	switch spec.Mode {
	case GateModeOpen:
		g.since = time.Time{}
		return Decision{Admit: true, Reason: ReasonAdmitted}
	case GateModeAppMarker:
		ok, reason := AdmitAppMarker(app, thisDC, maxAge, now)
		g.since = time.Time{}
		return Decision{Admit: ok, Reason: reason}
	case GateModeTiers:
		if len(obs) != len(spec.Tiers) {
			g.since = time.Time{}
			return Decision{Reason: ReasonClosed}
		}
		for i, t := range spec.Tiers {
			if ok, reason := AdmitTier(t, obs[i], thisDC, maxAge, now); !ok {
				reasons = append(reasons, reason)
			}
		}
	default: // Closed and anything unknown
		g.since = time.Time{}
		return Decision{Reason: ReasonClosed}
	}

	if len(reasons) == 0 {
		g.since = time.Time{}
		return Decision{Admit: true, Reason: ReasonAdmitted}
	}

	// Graceable only when the sole failing check, everywhere, is C3.
	graceable := true
	for _, r := range reasons {
		if r != ReasonNoPrimaryHere {
			graceable = false
		}
	}
	if !graceable || !wasAdmitted || grace <= 0 {
		g.since = time.Time{}
		return Decision{Reason: reasons[0]}
	}
	if g.since.IsZero() {
		g.since = now
	}
	if left := grace - now.Sub(g.since); left > 0 {
		return Decision{Admit: true, Reason: ReasonAdmitted, Holding: true, RecheckIn: left}
	}
	g.since = time.Time{}
	return Decision{Reason: ReasonNoPrimaryHere}
}

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
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

const here = "spoke-1"

func pgTier() GateTier {
	return GateTier{Lease: "primary-dc-pg-docs", Namespace: "demo", Name: "pg-docs", Kind: "Postgres", Camp: CampSingleWriter, Service: "pg-docs"}
}

// fresh is a marker that names dc, renewed age ago.
func fresh(dc string, age time.Duration) Marker {
	return Marker{Present: true, ActiveDC: dc, Renew: t0.Add(-age)}
}

// TestAdmitTier has one case per row of the design's check table (5.1), plus the
// ways each can fail. It is the table the safety argument rests on.
func TestAdmitTier(t *testing.T) {
	maxAge := 30 * time.Second
	cases := []struct {
		name   string
		tier   GateTier
		obs    TierObservation
		want   bool
		reason string
	}{
		{"C1 C2 C3 hold", pgTier(), TierObservation{Marker: fresh(here, 5*time.Second), PrimaryReady: true}, true, ReasonAdmitted},
		{"marker exactly at the limit is fresh", pgTier(), TierObservation{Marker: fresh(here, 30*time.Second), PrimaryReady: true}, true, ReasonAdmitted},

		{"C1: no marker", pgTier(), TierObservation{PrimaryReady: true}, false, ReasonMarkerMissing},
		{"C1: marker one second past the limit", pgTier(), TierObservation{Marker: fresh(here, 31*time.Second), PrimaryReady: true}, false, ReasonMarkerStale},
		{"C1: marker present but renewTime unparsable", pgTier(), TierObservation{Marker: Marker{Present: true, ActiveDC: here}, PrimaryReady: true}, false, ReasonMarkerStale},
		{"C1: marker renewed in the future (clock skew)", pgTier(), TierObservation{Marker: fresh(here, -time.Hour), PrimaryReady: true}, false, ReasonMarkerStale},
		{"C1: marker names another DC", pgTier(), TierObservation{Marker: fresh("spoke-2", time.Second), PrimaryReady: true}, false, ReasonNotActiveHere},
		{"C1: marker names no DC", pgTier(), TierObservation{Marker: fresh("", time.Second), PrimaryReady: true}, false, ReasonNotActiveHere},
		{"C1: stale marker for another DC is stale, not 'elsewhere'", pgTier(), TierObservation{Marker: fresh("spoke-2", time.Hour), PrimaryReady: true}, false, ReasonMarkerStale},

		{"C2: a planned switchover quiesces this DC", pgTier(), TierObservation{Marker: Marker{Present: true, ActiveDC: here, Renew: t0, Quiesce: here}, PrimaryReady: true}, false, ReasonQuiescing},
		{"C2: quiesce naming another DC does not matter here", pgTier(), TierObservation{Marker: Marker{Present: true, ActiveDC: here, Renew: t0, Quiesce: "spoke-2"}, PrimaryReady: true}, true, ReasonAdmitted},

		{"C3: no writable primary here", pgTier(), TierObservation{Marker: fresh(here, time.Second)}, false, ReasonNoPrimaryHere},

		{"C4: break-glass override replaces C1 (marker missing)", pgTier(), TierObservation{OverrideActive: true, PrimaryReady: true}, true, ReasonAdmitted},
		{"C4: override with another DC in the marker", pgTier(), TierObservation{OverrideActive: true, Marker: fresh("spoke-2", time.Second), PrimaryReady: true}, true, ReasonAdmitted},
		{"C4: override does not replace C3", pgTier(), TierObservation{OverrideActive: true}, false, ReasonNoPrimaryHere},
		{"C4: override does not replace C2", pgTier(), TierObservation{OverrideActive: true, Marker: Marker{Present: true, Quiesce: here}, PrimaryReady: true}, false, ReasonQuiescing},

		// Camps. Only Leaderless is relaxed; the unvalidated camps stay restrictive.
		{"leaderless: any local instance", GateTier{Camp: CampLeaderlessGeoNative}, TierObservation{AnyInstanceReady: true}, true, ReasonAdmitted},
		{"leaderless: no local instance", GateTier{Camp: CampLeaderlessGeoNative}, TierObservation{Marker: fresh(here, time.Second)}, false, ReasonNoInstance},
		{"leaderless: marker is not consulted", GateTier{Camp: CampLeaderlessGeoNative}, TierObservation{AnyInstanceReady: true, Marker: fresh("spoke-2", time.Hour)}, true, ReasonAdmitted},
		{"async mirror needs the marker", GateTier{Camp: CampAsyncMirror}, TierObservation{PrimaryReady: true}, false, ReasonMarkerMissing},
		{"quorum native needs the marker", GateTier{Camp: CampQuorumNative}, TierObservation{PrimaryReady: true, Marker: fresh("spoke-2", time.Second)}, false, ReasonNotActiveHere},
		{"an unknown camp is treated as a single writer", GateTier{Camp: "SomethingNew"}, TierObservation{PrimaryReady: true}, false, ReasonMarkerMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := AdmitTier(tc.tier, tc.obs, here, maxAge, t0)
			if got != tc.want || reason != tc.reason {
				t.Fatalf("AdmitTier() = (%v, %q), want (%v, %q)", got, reason, tc.want, tc.reason)
			}
		})
	}
}

func TestAdmitAppMarker(t *testing.T) {
	maxAge := 30 * time.Second
	cases := []struct {
		name   string
		obs    TierObservation
		want   bool
		reason string
	}{
		{"fresh and here", TierObservation{Marker: fresh(here, time.Second)}, true, ReasonAdmitted},
		{"missing", TierObservation{}, false, ReasonMarkerMissing},
		{"stale", TierObservation{Marker: fresh(here, time.Minute)}, false, ReasonMarkerStale},
		{"another DC", TierObservation{Marker: fresh("spoke-2", time.Second)}, false, ReasonNotActiveHere},
		{"quiescing", TierObservation{Marker: Marker{Present: true, ActiveDC: here, Renew: t0, Quiesce: here}}, false, ReasonQuiescing},
		{"override", TierObservation{OverrideActive: true}, true, ReasonAdmitted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := AdmitAppMarker(tc.obs, here, maxAge, t0)
			if got != tc.want || reason != tc.reason {
				t.Fatalf("AdmitAppMarker() = (%v, %q), want (%v, %q)", got, reason, tc.want, tc.reason)
			}
		})
	}
}

func TestEffectiveMaxAge(t *testing.T) {
	for in, want := range map[int32]time.Duration{0: 30 * time.Second, -5: 30 * time.Second, 10: 10 * time.Second, 30: 30 * time.Second, 300: 30 * time.Second} {
		if got := EffectiveMaxAge(GateSpec{MarkerMaxAgeSeconds: in}); got != want {
			t.Errorf("EffectiveMaxAge(%d) = %v, want %v (the window may only shrink)", in, got, want)
		}
	}
}

func tiersSpec(grace int32, tiers ...GateTier) GateSpec {
	return GateSpec{Version: SpecVersion, Mode: GateModeTiers, Tiers: tiers, PrimaryLossGraceSeconds: grace}
}

// TestEvaluateHysteresis is design 5.4: only C3 is graced, only while admitted,
// and every other loss revokes at once.
func TestEvaluateHysteresis(t *testing.T) {
	spec := tiersSpec(15, pgTier())
	// The marker is renewed every few seconds in reality, so build it at the
	// time of each evaluation.
	at := func(d time.Duration, primary bool) ([]TierObservation, time.Time) {
		now := t0.Add(d)
		return []TierObservation{{Marker: Marker{Present: true, ActiveDC: here, Renew: now.Add(-time.Second)}, PrimaryReady: primary}}, now
	}
	g := &Grace{}

	obs, now := at(0, true)
	if d := Evaluate(spec, obs, TierObservation{}, here, true, g, now); !d.Admit || d.Holding {
		t.Fatalf("steady: %+v", d)
	}
	// The primary pod dies: the site stays admitted while the local raft re-elects.
	obs, now = at(0, false)
	if d := Evaluate(spec, obs, TierObservation{}, here, true, g, now); !d.Admit || !d.Holding || d.RecheckIn != 15*time.Second {
		t.Fatalf("t+0 primary lost: %+v", d)
	}
	obs, now = at(6*time.Second, false)
	if d := Evaluate(spec, obs, TierObservation{}, here, true, g, now); !d.Admit || !d.Holding || d.RecheckIn != 9*time.Second {
		t.Fatalf("t+6 still holding: %+v", d)
	}
	// A new primary within the grace: admitted, and the grace is forgotten.
	obs, now = at(8*time.Second, true)
	if d := Evaluate(spec, obs, TierObservation{}, here, true, g, now); !d.Admit || d.Holding {
		t.Fatalf("recovered: %+v", d)
	}
	// A second loss gets a fresh grace, not the remainder of the first.
	obs, now = at(40*time.Second, false)
	if d := Evaluate(spec, obs, TierObservation{}, here, true, g, now); !d.Holding || d.RecheckIn != 15*time.Second {
		t.Fatalf("second loss must restart the grace: %+v", d)
	}
	// The grace runs out: revoked.
	obs, now = at(56*time.Second, false)
	if d := Evaluate(spec, obs, TierObservation{}, here, true, g, now); d.Admit || d.Reason != ReasonNoPrimaryHere {
		t.Fatalf("grace over: %+v", d)
	}
}

func TestEvaluateOnlyPrimaryLossIsGraced(t *testing.T) {
	spec := tiersSpec(15, pgTier())
	for name, o := range map[string]TierObservation{
		"marker gone":                {PrimaryReady: true},
		"marker stale":               {Marker: fresh(here, time.Minute), PrimaryReady: true},
		"other DC active":            {Marker: fresh("spoke-2", time.Second), PrimaryReady: true},
		"quiesce":                    {Marker: Marker{Present: true, ActiveDC: here, Renew: t0, Quiesce: here}, PrimaryReady: true},
		"marker gone and no primary": {},
	} {
		t.Run(name, func(t *testing.T) {
			d := Evaluate(spec, []TierObservation{o}, TierObservation{}, here, true, &Grace{}, t0)
			if d.Admit || d.Holding {
				t.Fatalf("must revoke at once, got %+v", d)
			}
		})
	}
	// A site that is not admitted is never admitted by the grace.
	if d := Evaluate(spec, []TierObservation{{Marker: fresh(here, time.Second)}}, TierObservation{}, here, false, &Grace{}, t0); d.Admit {
		t.Fatalf("an unadmitted site must not be admitted on a lost primary: %+v", d)
	}
	// Grace disabled.
	if d := Evaluate(tiersSpec(0, pgTier()), []TierObservation{{Marker: fresh(here, time.Second)}}, TierObservation{}, here, true, &Grace{}, t0); d.Admit {
		t.Fatalf("grace 0 must revoke at once: %+v", d)
	}
	// Two tiers: one graceable failure and one hard failure is a hard failure.
	two := tiersSpec(15, pgTier(), GateTier{Lease: "primary-dc-other", Service: "other", Camp: CampSingleWriter})
	obs := []TierObservation{{Marker: fresh(here, time.Second)}, {PrimaryReady: true}}
	if d := Evaluate(two, obs, TierObservation{}, here, true, &Grace{}, t0); d.Admit || d.Holding {
		t.Fatalf("a hard failure in one tier must not be graced because another is: %+v", d)
	}
}

func TestEvaluateRecoveryIsImmediate(t *testing.T) {
	spec := tiersSpec(15, pgTier())
	d := Evaluate(spec, []TierObservation{{Marker: fresh(here, time.Second), PrimaryReady: true}}, TierObservation{}, here, false, &Grace{}, t0)
	if !d.Admit {
		t.Fatalf("a recovered tier admits on the next evaluation: %+v", d)
	}
}

// TestEvaluateClosedByDefault is review R2: nothing to check is never open.
func TestEvaluateClosedByDefault(t *testing.T) {
	healthy := []TierObservation{{Marker: fresh(here, time.Second), PrimaryReady: true}}
	cases := map[string]GateSpec{
		"closed mode":          {Version: SpecVersion, Mode: GateModeClosed},
		"unknown mode":         {Version: SpecVersion, Mode: "Whatever"},
		"empty mode":           {Version: SpecVersion},
		"tiers mode, no tiers": {Version: SpecVersion, Mode: GateModeTiers},
		"zero value":           {},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if d := Evaluate(spec, healthy, TierObservation{}, here, false, &Grace{}, t0); d.Admit {
				t.Fatalf("must be closed, got %+v", d)
			}
		})
	}
	// Mismatched observations (a programming error) also close the gate.
	if d := Evaluate(tiersSpec(15, pgTier()), nil, TierObservation{}, here, true, &Grace{}, t0); d.Admit {
		t.Fatalf("missing observations must close the gate: %+v", d)
	}
	// Only an explicit Open admits without checks.
	if d := Evaluate(GateSpec{Version: SpecVersion, Mode: GateModeOpen}, nil, TierObservation{}, here, false, &Grace{}, t0); !d.Admit {
		t.Fatalf("Open must admit: %+v", d)
	}
}

func TestParseSpec(t *testing.T) {
	valid := `{"version":1,"mode":"Tiers","tiers":[{"lease":"primary-dc-pg","namespace":"demo","name":"pg","kind":"Postgres","camp":"SingleWriter","service":"pg"}],"markerMaxAgeSeconds":30,"primaryLossGraceSeconds":15}`
	s := ParseSpec(valid, true)
	if s.Mode != GateModeTiers || len(s.Tiers) != 1 || s.Tiers[0].Service != "pg" || s.PrimaryLossGraceSeconds != 15 {
		t.Fatalf("valid spec misparsed: %+v", s)
	}
	closed := map[string]struct {
		raw     string
		present bool
	}{
		"absent":               {"", false},
		"empty":                {"", true},
		"whitespace":           {"  ", true},
		"not json":             {"nope", true},
		"unknown version":      {`{"version":2,"mode":"Open"}`, true},
		"version zero":         {`{"mode":"Open"}`, true},
		"unknown mode":         {`{"version":1,"mode":"Maybe"}`, true},
		"tiers with no tiers":  {`{"version":1,"mode":"Tiers"}`, true},
		"tier with no lease":   {`{"version":1,"mode":"Tiers","tiers":[{"service":"pg"}]}`, true},
		"tier with no service": {`{"version":1,"mode":"Tiers","tiers":[{"lease":"l"}]}`, true},
		"app marker no lease":  {`{"version":1,"mode":"AppMarker"}`, true},
	}
	for name, in := range closed {
		t.Run(name, func(t *testing.T) {
			if got := ParseSpec(in.raw, in.present); got.Mode != GateModeClosed {
				t.Fatalf("must parse as Closed, got %+v", got)
			}
		})
	}
	if got := ParseSpec(`{"version":1,"mode":"Open"}`, true); got.Mode != GateModeOpen {
		t.Fatalf("an explicit Open is Open: %+v", got)
	}
	// Round trip.
	enc, err := tiersSpec(15, pgTier()).Encode()
	if err != nil || !strings.Contains(enc, `"mode":"Tiers"`) {
		t.Fatalf("Encode: %q %v", enc, err)
	}
	if back := ParseSpec(enc, true); back.Mode != GateModeTiers || back.Tiers[0].Lease != "primary-dc-pg-docs" {
		t.Fatalf("round trip: %+v", back)
	}
}

func TestParseMarker(t *testing.T) {
	m := ParseMarker(map[string]string{"activeDC": "spoke-1", "renewTime": "2026-11-13T12:00:00Z", "quiesce": "spoke-1"})
	if !m.Present || m.ActiveDC != "spoke-1" || !m.Renew.Equal(t0) || m.Quiesce != "spoke-1" {
		t.Fatalf("%+v", m)
	}
	if m := ParseMarker(map[string]string{"activeDC": "spoke-1", "renewTime": "2026-11-13T12:00:00.123456789Z"}); m.Renew.IsZero() {
		t.Fatal("RFC3339Nano must parse")
	}
	if m := ParseMarker(map[string]string{"activeDC": "spoke-1", "renewTime": "yesterday"}); !m.Renew.IsZero() {
		t.Fatal("an unparsable renewTime must leave Renew zero (stale)")
	}
}

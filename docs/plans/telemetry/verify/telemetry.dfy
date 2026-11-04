// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Verified models of pure functions in ate_env_rl telemetry.
// Spec: ../interfaces.md, ../acceptance-criteria.md (AC-4, AC-7, AC-9), ../properties.md.
// Each `ensures` is tagged with the spec property it proves (P-n).

module Telemetry {

  // ---------------------------------------------------------------------
  // P-1..P-3 (AC-9): `family` label cardinality is bounded.
  // Models observability.repo_family's reserved-name rename followed by
  // observability._bounded_family (amended 2026-10-23, see decisions.md).
  // ---------------------------------------------------------------------
  const MAX_FAMILIES: nat := 64
  const OTHER: string := "other"
  const UNKNOWN: string := "unknown"

  predicate Reserved(s: string) { s == OTHER || s == UNKNOWN }

  // A repo whose family is a reserved name is labelled `<name>_repo`.
  function Unreserve(candidate: string): (r: string)
    ensures !Reserved(r)
  {
    if Reserved(candidate) then candidate + "_repo" else candidate
  }

  method BoundedFamily(families: set<string>, candidate: string)
      returns (lbl: string, families': set<string>)
    requires |families| <= MAX_FAMILIES
    requires forall f :: f in families ==> !Reserved(f)
    ensures |families'| <= MAX_FAMILIES                                // P-1: registry stays bounded
    ensures forall f :: f in families' ==> !Reserved(f)                //      reserved names never use a slot
    ensures families <= families'                                      //      never forgets a family
    ensures lbl == Unreserve(candidate) || lbl == OTHER                // P-2: label is the family or "other"
    ensures lbl == OTHER ==> (|families| == MAX_FAMILIES && Unreserve(candidate) !in families)  // P-3: "other" only on overflow
    ensures lbl != OTHER ==> lbl in families'
    // Before the amendment this model had no Unreserve step, and P-3 failed with the
    // counter-example candidate == "other". Delete the Unreserve call to see it fail.
  {
    var name := Unreserve(candidate);
    if name in families {
      return name, families;
    }
    if |families| >= MAX_FAMILIES {
      return OTHER, families;
    }
    return name, families + {name};
  }

  lemma SubsetCard<T>(a: set<T>, b: set<T>)
    requires a <= b
    ensures |a| <= |b|
  {
    if a != {} {
      var x :| x in a;
      SubsetCard(a - {x}, b - {x});
    }
  }

  // Whole-run theorem: whatever stream of repos/images a run sees, the number
  // of distinct `family` label values ever emitted is at most MAX_FAMILIES + 1.
  // The model covers parseable inputs only (every name has a repo family).
  // In the code, an unparseable input is labelled "unknown", which is not
  // registered and does not count toward the cap, so the bound over all inputs
  // is MAX_FAMILIES + 2: the families, "other" and "unknown" (amended
  // 2026-10-27 after the PBT run; see properties.md P-1, AC-9.1).
  method LabelStream(names: seq<string>) returns (labels: seq<string>)
    ensures |labels| == |names|
    ensures forall i :: 0 <= i < |labels| ==> labels[i] == Unreserve(names[i]) || labels[i] == OTHER
    ensures |set i | 0 <= i < |labels| :: labels[i]| <= MAX_FAMILIES + 1   // P-1 over a run
  {
    var fams: set<string> := {};
    labels := [];
    for k := 0 to |names|
      invariant |labels| == k
      invariant |fams| <= MAX_FAMILIES
      invariant forall f :: f in fams ==> !Reserved(f)
      invariant forall i :: 0 <= i < k ==> labels[i] == Unreserve(names[i]) || labels[i] == OTHER
      invariant forall i :: 0 <= i < k ==> labels[i] in fams || labels[i] == OTHER
    {
      var l;
      l, fams := BoundedFamily(fams, names[k]);
      labels := labels + [l];
    }
    var emitted := set i | 0 <= i < |labels| :: labels[i];
    SubsetCard(emitted, fams + {OTHER});
  }

  // ---------------------------------------------------------------------
  // P-4..P-7 (AC-4): inject_metadata puts the W3C context on the wire.
  // Models telemetry.tracing.inject_metadata.
  // ---------------------------------------------------------------------
  type Metadata = seq<(string, string)>

  function LowerChar(c: char): char {
    if 'A' <= c <= 'Z' then (c as int + 32) as char else c
  }
  function Lower(s: string): string {
    seq(|s|, i requires 0 <= i < |s| => LowerChar(s[i]))
  }

  const CARRIER_KEYS: set<string> := {"traceparent", "tracestate"}

  predicate KeyIn(md: Metadata, lk: string) {
    exists j :: 0 <= j < |md| && Lower(md[j].0) == lk
  }

  method InjectMetadata(md: Metadata, carrier: Metadata) returns (out: Metadata)
    // What _current_carrier guarantees: a dict filtered to _CARRIER_KEYS.
    requires forall i :: 0 <= i < |carrier| ==> carrier[i].0 in CARRIER_KEYS
    requires forall i, j :: 0 <= i < j < |carrier| ==> carrier[i].0 != carrier[j].0
    ensures |out| >= |md| && out[..|md|] == md                               // P-4: caller metadata kept, in order
    ensures forall i :: |md| <= i < |out| ==> out[i] in carrier              // P-5: only carrier entries added
    ensures forall i :: 0 <= i < |out| ==> out[i].0 != "baggage" || out[i] in md  // P-6: never adds baggage
    ensures forall i :: 0 <= i < |carrier| ==> KeyIn(out, carrier[i].0)      // P-7: every carrier key present
    ensures forall a, b :: |md| <= a < |out| && 0 <= b < |out| && a != b
              ==> Lower(out[a].0) != Lower(out[b].0)                         //      ...and never doubled
  {
    var present := set j | 0 <= j < |md| :: Lower(md[j].0);
    out := md;
    for i := 0 to |carrier|
      invariant |out| >= |md| && out[..|md|] == md
      invariant forall a :: |md| <= a < |out| ==> out[a] in carrier[..i]
      invariant forall a :: |md| <= a < |out| ==> Lower(out[a].0) == out[a].0 && out[a].0 !in present
      invariant forall a, b :: |md| <= a < b < |out| ==> out[a].0 != out[b].0
      invariant forall k :: 0 <= k < i ==> KeyIn(out, carrier[k].0)
    {
      var (key, value) := carrier[i];
      assert key == "traceparent" || key == "tracestate";
      assert Lower(key) == key;
      ghost var prev := out;
      if key !in present {
        out := out + [(key, value)];
        forall k | 0 <= k < i ensures KeyIn(out, carrier[k].0) {
          var j :| 0 <= j < |prev| && Lower(prev[j].0) == carrier[k].0;
          assert out[j] == prev[j];
        }
        assert Lower(out[|out| - 1].0) == key;
      } else {
        var j :| 0 <= j < |md| && Lower(md[j].0) == key;
        assert out[j] == md[j];
      }
      assert KeyIn(out, key);
    }
    forall a, b | |md| <= a < |out| && 0 <= b < |out| && a != b
      ensures Lower(out[a].0) != Lower(out[b].0)
    {
      if b < |md| {
        assert out[b] == md[b];
        assert Lower(md[b].0) in present;
      }
    }
  }

  // ---------------------------------------------------------------------
  // P-8 (AC-7): RunReport.phases entries stay internally consistent.
  // Models RunReport.add_phase with the negative-duration clamp
  // (amended 2026-10-23). `real` is exact; Python uses float.
  // ---------------------------------------------------------------------
  datatype PhaseStat = PhaseStat(count: nat, total: real, max: real)

  predicate ValidStat(p: PhaseStat) {
    && 0.0 <= p.max <= p.total
    && (p.count == 0 ==> p.total == 0.0)
    && p.total <= p.count as real * p.max
  }

  lemma MulMono(k: real, a: real, b: real)
    requires k >= 0.0 && a <= b
    ensures k * a <= k * b
  {
    assert k * b - k * a == k * (b - a);
  }

  method AddPhase(phases: map<string, PhaseStat>, name: string, dur: real)
      returns (phases': map<string, PhaseStat>)
    // Before the amendment this needed `requires dur >= 0.0`: without it P-8 fails
    // (add_phase(x, 2.0); add_phase(x, -5.0) gives total -3.0, max 2.0).
    requires forall n :: n in phases ==> ValidStat(phases[n])
    ensures forall n :: n in phases' ==> ValidStat(phases'[n])               // P-8
    ensures name in phases' && phases'[name].count == (if name in phases then phases[name].count else 0) + 1
    ensures forall n :: n in phases && n != name ==> n in phases' && phases'[n] == phases[n]
  {
    var d := if dur < 0.0 then 0.0 else dur;   // clamp
    var c := if name in phases then phases[name] else PhaseStat(0, 0.0, 0.0);
    var m := if c.max >= d then c.max else d;
    var n := PhaseStat(c.count + 1, c.total + d, m);
    assert ValidStat(c);
    assert 0.0 <= d <= m;
    MulMono(c.count as real, c.max, m);
    assert c.total <= c.count as real * m;
    assert c.total + d <= c.count as real * m + m;
    assert (c.count + 1) as real * m == c.count as real * m + m;
    assert ValidStat(n);
    phases' := phases[name := n];
  }
}

package normalization

// This file implements dimensional (scale-only) unit conversion —
// deliberately NOT molar-mass-dependent conversion (e.g. mmol/L <-> mg/dL
// for a specific analyte like glucose or cholesterol, where the factor
// depends on that substance's molecular weight). That half of the problem
// is coupled to canonicalize's still-open gap (you need to know WHICH
// analyte before you can look up its molar mass) and is out of scope here.
//
// unitEquivalenceGroups was seeded from real, observed unit pairs — not a
// general SI/dimensional-analysis engine — see
// internal/extraction/testdata/invitro_cbc.txt vs.
// helix_biochem_lipid_cbc.txt for the two concrete lab reports that
// motivated this (same indicators — Гемоглобин, Эритроциты, Лейкоциты —
// reported in different units by two different labs). Grows the same way
// the fixtures do: add a group when a real document exercises a pair not
// covered yet, not by trying to anticipate every unit in advance.

// unitEquivalenceGroups lists sets of units known to represent the same
// physical quantity. Each value is that unit's factor relative to an
// arbitrary reference unit within its own group (the reference itself is
// an implementation detail of convertUnit's arithmetic — it has no
// relationship to which unit ends up as any given user's canonical unit
// for an indicator; see CanonicalUnitResolver for that).
var unitEquivalenceGroups = []map[string]float64{
	// Mass concentration (e.g. Гемоглобин). Reference: г/л.
	// 1 г/дл = 1 g / 0.1 L = 10 g/L.
	{"г/л": 1, "г/дл": 10},

	// Cell count concentration, "10^N клеток/л" written out longhand vs.
	// the compact тыс/млн-per-мкл form labs also use. These are exactly
	// equal (not just proportional), so both factors are 1 — see this
	// file's package doc comment for the arithmetic.
	{"10^12 клеток/л": 1, "млн/мкл": 1},
	{"10^9 клеток/л": 1, "тыс/мкл": 1},
}

// convertUnit converts value from fromUnit to toUnit if both are known to
// belong to the same dimensional equivalence group. ok is false if the
// units aren't recognized as equivalent — callers must not guess a factor
// in that case (same "don't fabricate" posture as the rest of this
// package/extraction — see canonicalize's doc comment for the analogous
// call on names).
func convertUnit(value float64, fromUnit, toUnit string) (converted float64, ok bool) {
	if fromUnit == toUnit {
		return value, true
	}
	for _, group := range unitEquivalenceGroups {
		fromFactor, fromOK := group[fromUnit]
		toFactor, toOK := group[toUnit]
		if fromOK && toOK {
			return value * fromFactor / toFactor, true
		}
	}
	return 0, false
}

// CanonicalUnitResolver looks up the unit a user's given indicator has
// already been normalized to, if this isn't the first time it's been seen.
// Implemented outside this package (backed by a real Repository query, e.g.
// "the unit of this (userID, indicatorName)'s earliest-processed
// LabResult") — this package only needs the narrow read, never writes
// through it.
//
// Why Normalize needs this at all, despite otherwise being strictly
// document-scoped (see this package's doc comment): unlike cross-document
// entity linking (deliberately never persisted, see
// docs/architecture/02-processing-pipeline.md §6), picking a canonical unit
// is a purely mechanical, deterministic lookup with no LLM/semantic
// judgment involved — reprocessing the same document against the same
// database state always yields the same conversion. The result
// (NormalizedValue/NormalizedUnit) is still treated as a rebuildable
// derived cache, not a permanent fact — see docs/domain/09-lab-result-and-vital-sign.md
// §7 for the operational consequence of that (what happens if the
// document that established a canonical unit is later deleted).
type CanonicalUnitResolver interface {
	// CanonicalUnit returns the previously-established canonical unit for
	// (userID, indicatorName) and true, or ("", false) if this indicator
	// has never been recorded for this user before now.
	CanonicalUnit(userID, indicatorName string) (unit string, found bool)
}

// normalizeUnit resolves (value, unit)'s canonical form for
// (userID, indicatorName) against resolver. Three outcomes:
//   - No canonical unit exists yet for this indicator: this measurement's
//     own unit becomes canonical by definition (NormalizedValue/Unit equal
//     Value/Unit unchanged).
//   - A canonical unit exists and convertUnit recognizes the pair: returns
//     the converted value in that unit.
//   - A canonical unit exists but convertUnit doesn't recognize the pair
//     (an exotic or not-yet-catalogued unit): returns ("", 0, false) —
//     callers must leave NormalizedValue/NormalizedUnit unset rather than
//     guess, exactly like an unparseable date is left nil rather than
//     defaulted (see parseOptionalDate).
func normalizeUnit(resolver CanonicalUnitResolver, userID, indicatorName string, value float64, unit string) (normalizedValue float64, normalizedUnit string, ok bool) {
	if unit == "" {
		return 0, "", false
	}
	canonical, found := resolver.CanonicalUnit(userID, indicatorName)
	if !found {
		return value, unit, true
	}
	converted, convOK := convertUnit(value, unit, canonical)
	if !convOK {
		return 0, "", false
	}
	return converted, canonical, true
}

package normalization

import "strings"

// This file implements a small, hand-curated LOINC lookup used only as a
// fallback when a LabResult's code wasn't transcribed directly from the
// source document (extraction.LabResult.Code/CodeSystem — the model copies
// a code only if it's literally printed, the same rule already used for
// Diagnosis/ICD-10, see extraction.Schema's diagnoses.code description).
// Transcription alone rarely helps here: unlike ICD-10 (routinely printed
// on Russian consultation notes), Russian labs essentially never print
// LOINC on a result sheet — so without this fallback, LabResult.Code would
// stay empty on almost every real document.
//
// IMPORTANT — accuracy caveat, read before trusting or extending this file:
// unlike units.go's conversion factors (pure arithmetic, verifiable by
// anyone reading the code), the codes below are LOINC's own external
// identifiers. They were entered from general knowledge, not cross-checked
// against an authoritative LOINC release file or loinc.org at the time this
// was written. This table is a reasonable-confidence starting point for a
// handful of extremely common, unambiguous tests — it is NOT a verified
// reference, and every entry should get a human spot-check against
// loinc.org before being relied on for anything beyond an informational
// label. Deliberately kept small for this reason: entries were added only
// for indicators that actually appear in this project's own fixtures
// (internal/extraction/testdata), not speculatively for broad coverage.
var loincByAlias = map[string]string{
	"гемоглобин":       "718-7",
	"hgb":              "718-7",
	"гемоглобин (hgb)": "718-7",

	"гематокрит":       "4544-3",
	"hct":              "4544-3",
	"гематокрит (hct)": "4544-3",

	"лейкоциты":       "6690-2",
	"wbc":             "6690-2",
	"лейкоциты (wbc)": "6690-2",

	"эритроциты":       "789-8",
	"rbc":              "789-8",
	"эритроциты (rbc)": "789-8",

	"тромбоциты":       "777-3",
	"plt":              "777-3",
	"тромбоциты (plt)": "777-3",

	"аланинаминотрансфераза": "1742-6",
	"алт": "1742-6",
	"аланинаминотрансфераза (алт)": "1742-6",

	"аспартатаминотрансфераза": "1920-8",
	"аст": "1920-8",
	"аспартатаминотрансфераза (аст)": "1920-8",

	"креатинин": "2160-0",

	"билирубин общий": "1975-2",

	"общий белок": "2885-2",

	"холестерин": "2093-3",
}

// lookupLOINC returns a best-effort LOINC code for name — case-insensitive,
// trimmed, exact match against loincByAlias only. No fuzzy matching and no
// attempt to strip or interpret parenthetical abbreviations beyond the
// literal aliases enumerated above: a general "strip anything in parens"
// heuristic would be wrong as often as right (some parentheticals are
// abbreviations, others are qualifiers like "(общ.число)" — see
// canonicalize's doc comment for the same reasoning applied to names in
// general).
func lookupLOINC(name string) (code string, ok bool) {
	code, ok = loincByAlias[strings.ToLower(strings.TrimSpace(name))]
	return code, ok
}

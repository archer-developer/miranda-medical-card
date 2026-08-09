package normalization

import (
	"context"
	"strings"
)

// This file is the indicator-name counterpart of loinc.go's alias lookup —
// same reasoning and accuracy posture, applied to LabResult.IndicatorName
// instead of LabResult.Code, but backed by a database table
// (storage.IndicatorAliasRepository) rather than a Go literal: unlike
// loincByAlias, this dictionary is meant to keep growing at runtime (an
// operator editing it directly, or eventually an LLM fallback step) without
// requiring a new deploy every time.
//
// Why this exists at all: canonicalize's own doc comment called this out as
// the specific open gap ("matching different spellings of the same
// indicator") — see docs/architecture/02-processing-pipeline.md §6. Without
// it, "Лимфоциты (LYMPH)", "Лимфоциты, абс." and "LYMPH" land as three
// unrelated indicator names, so HistoryByIndicator/LatestByIndicator (see
// storage.LabResultRepository) can never group them into one trend.
//
// IMPORTANT — the one mistake this dictionary must never make: conflating a
// relative (%) and an absolute (count) variant of the same cell type. Real
// production data looks like:
//
//	'Лимфоциты (LYMPH%)'  | %
//	'Лимфоциты, %'        | %
//	'Лимфоциты (LYMPH)'   | 10^9 клеток/л
//	'Лимфоциты, абс.'     | тыс/мкл
//
// These are two different measurements, not four spellings of one — a naive
// "strip everything after the comma/paren and compare" heuristic would
// silently merge 30% and 30×10⁹/л under one name. Whoever adds a row to
// indicator_aliases (human or LLM) must keep every %/absolute pair as two
// distinct canonical entries for exactly this reason; unit is the
// discriminator, not wording.
//
// Deliberately never aliased on name alone, even when two rows look
// related — left as separate, unaliased names for a human to resolve with
// more context than a bare (name, unit) pair has:
//   - "Белок" vs "Общий белок" — could be the same serum total protein
//     test, or "Белок" could be urine protein from a urinalysis (different
//     fluid, different test).
//   - A lone "Гемоглобин"/"Билирубин" row with an empty unit — looks like
//     an incomplete extraction, not a naming variant; aliasing it wouldn't
//     fix the actual defect (a missing unit).

// IndicatorAliasResolver looks up the canonical spelling registered for a
// raw indicator name — implemented outside this package
// (storage.IndicatorAliasRepository, backed by the indicator_aliases
// table). Global, not scoped per user like CanonicalUnitResolver: an
// indicator's identity ("Лимфоциты (LYMPH)" means the same thing) doesn't
// depend on whose document it came from, only its own spelling.
type IndicatorAliasResolver interface {
	// CanonicalIndicatorName returns the canonical spelling registered for
	// alias (case-insensitive, trimmed exact match — see loinc.go's
	// lookupLOINC for why this package refuses fuzzy matching in general)
	// and true, or ("", false, nil) if alias isn't a known alias for
	// anything, which is the common case for most indicator names. err is
	// for a genuine lookup failure (e.g. a database error), never
	// conflated with "not found."
	CanonicalIndicatorName(ctx context.Context, alias string) (canonical string, found bool, err error)
}

// indicatorAliasSeedGroups is the initial, git-reviewed dictionary content
// used to seed the indicator_aliases table the first time this service
// runs against a given database (see cmd/miranda-medical-card/main.go's
// startup sequence, which calls SetIfAbsent for every alias in every
// group here) — after seeding, the database is authoritative and this list
// is never consulted directly by Normalize again. Seeded from indicator_name
// values actually observed in the deployed database (2026-08-09 audit),
// not speculatively — same growth policy as loincByAlias. Every entry was
// verified against that audit's (name, unit) pairs before being added
// here; new entries added directly to the database later don't need to be
// mirrored back into this list (it's a seed, not a mirror).
var indicatorAliasSeedGroups = [][]string{
	{"Лейкоциты", "Лейкоциты (WBC)"},
	{"Эритроциты", "Эритроциты (RBC)"},
	{"Тромбоциты", "Тромбоциты (PLT)"},
	{"Гемоглобин", "Гемоглобин (HGB)"},
	{"Гематокрит", "Гематокрит (HCT)"},

	{"Нейтрофилы, %", "Нейтрофилы (общ.число), %", "Нейтрофильные гранулоциты (NEUT %)"},
	{"Нейтрофилы, абс.", "Нейтрофильные гранулоциты (NEUT)"},
	{"Лимфоциты, %", "Лимфоциты (LYMPH%)"},
	{"Лимфоциты, абс.", "Лимфоциты (LYMPH)"},
	{"Моноциты, %", "Моноциты (MONO%)"},
	{"Моноциты, абс.", "Моноциты (MONO)"},
	{"Эозинофилы, %", "Эозинофилы (EO%)"},
	{"Эозинофилы, абс.", "Эозинофилы (EO)"},
	{"Базофилы, %", "Базофилы (BASO%)"},
	{"Базофилы, абс.", "Базофилы (BASO)"},

	{"АЛТ", "АлАТ", "Аланинаминотрансфераза (АЛТ)"},
	{"АСТ", "АсАТ", "Аспартатаминотрансфераза (АСТ)"},

	{"Глюкоза", "Глюкоза (сахар)", "Глюкоза (сыворотка)"},

	{"ТТГ", "Тиреотропный гормон (ТТГ)"},
	{"Т3 свободный", "Трийодтиронин свободный (Т3 св.)"},
	{"Т4 свободный", "Тироксин свободный (Т4 св.)"},

	{"Холестерин-ЛПВП", "ЛПВП-холестерин (HDL)"},
	{"Холестерин-ЛПНП", "Холестерин-ЛПНП (по Фридвальду)", "ЛПНП-холестерин (LDL)"},
	{"Коэффициент атерогенности", "Индекс атерогенности"},

	{"MCV", "Средний объем эритроцита (MCV)", "MCV (ср. объем эритр.)"},
	{"MCH", "Среднее содержание гемоглобина в эритроците (МСН)", "MCH (ср. содер. Hb в эр.)"},
	{"MCHC", "Средняя концентрация гемоглобина в эритроците (МСНС)", "MCHC (ср. конц. Hb в эр.)"},
	{"RDW-CV", "Ширина распределения эритроцитов (RDW-CV)", "RDW (шир. распред. эритр)"},

	{"СОЭ", "СОЭ (метод кинетики агрегации эритроцитов), венозная кровь"},
}

// IndicatorAliasSeedGroups exposes indicatorAliasSeedGroups outside this
// package — its only callers are cmd/miranda-medical-card/main.go
// (seeding a real deployment's database on startup) and
// cmd/renormalize-labs (the one-off migration tool that needed this same
// content before indicator_aliases existed as a table). Each inner slice
// is one group of interchangeable names; index 0 is that group's canonical
// spelling.
func IndicatorAliasSeedGroups() [][]string {
	return indicatorAliasSeedGroups
}

// canonicalizeIndicatorName trims name and, if resolver recognizes it as a
// known alias, rewrites it to the registered canonical spelling. A nil
// resolver (e.g. a caller that only wants the raw structural mapping, or a
// unit test uninterested in aliasing) skips lookup entirely and returns the
// trimmed name unchanged — the same "pass nil to skip it" contract
// CanonicalUnitResolver already uses. An unrecognized name also passes
// through unchanged: this function never invents a canonical form for a
// name nothing has registered.
func canonicalizeIndicatorName(ctx context.Context, resolver IndicatorAliasResolver, name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if resolver == nil || trimmed == "" {
		return trimmed, nil
	}
	canonical, found, err := resolver.CanonicalIndicatorName(ctx, trimmed)
	if err != nil {
		return trimmed, err
	}
	if found {
		return canonical, nil
	}
	return trimmed, nil
}

// CanonicalizeIndicatorName is canonicalizeIndicatorName exported for reuse
// outside this package — specifically cmd/renormalize-labs, which
// re-derives IndicatorName for LabResults directly against storage without
// re-running the full Extraction/Normalize pipeline (see that command's doc
// comment for why that's the right boundary: this is the exact same
// resolver-backed lookup Normalize itself uses, just invoked directly
// against already-persisted rows).
func CanonicalizeIndicatorName(ctx context.Context, resolver IndicatorAliasResolver, name string) (string, error) {
	return canonicalizeIndicatorName(ctx, resolver, name)
}

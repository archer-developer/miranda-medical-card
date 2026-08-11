// Command renormalize-labs is a one-off, standalone migration tool: it
// re-derives LabResult.IndicatorName for every LabResult already persisted
// before internal/normalization/indicator_aliases.go existed, using that
// file's alias dictionary, and recomputes NormalizedValue/NormalizedUnit
// and canonical_units to match.
//
// Deliberately NOT a re-run of the full Processing Pipeline
// (medical.reprocess_document / the administrative CLI reprocessing
// mentioned in docs/mcp/03-documents.md §3): both of those re-call OCR and
// Structured Extraction (LLM calls, non-deterministic, costly) to rebuild
// LabResults from scratch. Indicator-name canonicalization and unit
// conversion are both pure, deterministic functions of data already sitting
// in lab_results — there is nothing here an LLM call could improve, so
// this tool talks to internal/storage directly instead, the same
// "recompute a rebuildable derived value" category as
// docs/architecture/06-storage.md §15 describes for Domain Entities.
//
// Algorithm, per user (see run's doc comment for the exact steps):
//  1. Load every LabResult, compute its new canonical IndicatorName via
//     normalization.CanonicalizeIndicatorName.
//  2. Group by new IndicatorName; for each group, pick one canonical unit
//     (the unit of the group's earliest-TakenAt quantitative row) and
//     recompute NormalizedValue/NormalizedUnit for every row in the group
//     against it, via normalization.ConvertUnit — exactly the arithmetic
//     Normalize itself would use, just replayed across already-persisted
//     rows instead of one document's fresh extraction.
//  3. Apply changes one MedicalDocument at a time via
//     LabResultRepository.ReplaceForDocument — the same transactional
//     delete-and-recreate Normalize's own idempotency already relies on
//     (see internal/normalization's package doc comment), which is also
//     what makes intra-document duplicate removal safe: two rows from the
//     same document that, after renaming, are identical in
//     (IndicatorName, Value, Unit, TakenAt) are provably the same reported
//     fact under two spellings, not two real measurements, so the
//     redundant one is dropped. Nothing else is ever deleted — a
//     same-indicator match across two different documents is left alone
//     (it may be two real draws on different/same dates, which is
//     unrecoverable ambiguity from indicator_name alone) and reported for
//     manual review instead.
//  4. Overwrite canonical_units for every touched (userID, newName) via
//     CanonicalUnitRepository.Set, so future documents processed by the
//     live service resolve against the same unit this migration just used
//     — SetIfAbsent would refuse to update a stale entry left over from
//     before the rename.
//
// Usage:
//
//	renormalize-labs --db data/medical-card.db            # dry run, report only
//	renormalize-labs --db data/medical-card.db --apply     # write changes
//	renormalize-labs --db data/medical-card.db --user alex # scope to one user
//
// Always back up the database file before --apply — this tool does not do
// it for you (see docs/architecture/06-storage.md §13, and the operator's
// own runbook for how this deployment takes backups).
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	_ "modernc.org/sqlite"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/profile"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("renormalize-labs", flag.ExitOnError)
	dbPath := fs.String("db", "data/medical-card.db", "path to the SQLite database")
	apply := fs.Bool("apply", false, "write changes (default: dry-run report only)")
	onlyUser := fs.String("user", "", "restrict to a single userID (default: every user found in lab_results)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	userIDs, err := listLabResultUserIDs(*dbPath, *onlyUser)
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	if len(userIDs) == 0 {
		fmt.Println("no lab_results rows found — nothing to do")
		return nil
	}

	store, err := storage.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", *dbPath, err)
	}
	defer func() { _ = store.Close() }()

	labResults := storage.NewLabResultRepository(store)
	canonicalUnits := storage.NewCanonicalUnitRepository(store)
	indicatorAliases := storage.NewIndicatorAliasRepository(store)

	ctx := context.Background()

	// Seed indicator_aliases from the curated Go list before computing
	// anything — SetIfAbsent is idempotent and never overwrites a row
	// already there (from a prior seed, an operator, or an LLM), so this
	// runs unconditionally even in dry-run mode: the preview must reflect
	// the same dictionary --apply would use, or it isn't actually a
	// preview. This is the exact same seed cmd/miranda-medical-card/main.go
	// applies on every startup.
	for _, group := range normalization.IndicatorAliasSeedGroups() {
		canonical := group[0]
		for _, alias := range group {
			if err := indicatorAliases.SetIfAbsent(ctx, alias, canonical); err != nil {
				return fmt.Errorf("seed indicator alias %q: %w", alias, err)
			}
		}
	}

	// medical_profiles is a read-only cache (internal/pipeline.GetProfile
	// never rebuilds it, only reads — see internal/pipeline/reads.go)
	// normally refreshed as a side effect of every document upload
	// (docs/mcp/03-documents.md §4). Since this tool bypasses the Pipeline
	// entirely, any user whose lab_results actually changed needs their
	// Profile rebuilt here too, or medical.profile keeps serving
	// pre-migration indicator names until their next upload.
	profileBuilder := profile.NewBuilder(
		storage.NewMedicationRepository(store),
		storage.NewDiagnosisRepository(store),
		storage.NewProcedureRepository(store),
		storage.NewAllergyRepository(store),
		labResults,
		storage.NewVitalSignRepository(store),
		storage.NewDocumentRepository(store),
	)
	profileStore := profile.NewStore(storage.NewProfileRepository(store))

	var report report
	for _, userID := range userIDs {
		userReport, err := processUser(ctx, labResults, canonicalUnits, indicatorAliases, userID, *apply)
		if err != nil {
			return fmt.Errorf("process user %s: %w", userID, err)
		}
		report.merge(userReport)

		if *apply && (len(userReport.renames) > 0 || len(userReport.duplicatesRemoved) > 0) {
			rebuilt, err := profileBuilder.Build(ctx, userID)
			if err != nil {
				return fmt.Errorf("rebuild profile for %s: %w", userID, err)
			}
			if err := profileStore.Replace(ctx, rebuilt); err != nil {
				return fmt.Errorf("persist rebuilt profile for %s: %w", userID, err)
			}
			fmt.Printf("rebuilt medical_profiles cache for user %s\n", userID)
		}
	}

	report.print(*apply)
	return nil
}

// listLabResultUserIDs opens its own short-lived read-only connection —
// separate from storage.Open's single-writer-connection pool — purely to
// discover which userIDs actually have lab_results rows, since
// LabResultRepository has no such listing method (every other repository
// method here is intentionally scoped to one known userID, following
// docs/architecture/01-overview.md §4's closed-user-list model; this tool
// is the one place that legitimately needs "every user").
func listLabResultUserIDs(dbPath, onlyUser string) ([]string, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	if onlyUser != "" {
		return []string{onlyUser}, nil
	}

	rows, err := db.Query(`SELECT DISTINCT user_id FROM lab_results ORDER BY user_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// processUser implements the four-step algorithm from this file's package
// doc comment for one user, returning what happened (or would happen, in
// dry-run mode) for the final report.
func processUser(ctx context.Context, labResults storage.LabResultRepository, canonicalUnits storage.CanonicalUnitRepository, indicatorAliases storage.IndicatorAliasRepository, userID string, apply bool) (report, error) {
	var rep report

	all, err := labResults.ListByUser(ctx, userID)
	if err != nil {
		return rep, fmt.Errorf("list lab results: %w", err)
	}

	// Step 1+2: compute new names, group, pick one canonical unit per
	// group (earliest TakenAt among quantitative rows; nil TakenAt sorts
	// last — an undated row is never allowed to decide the canonical unit
	// for indicators that do have dated history).
	type indexed struct {
		row     normalization.LabResult
		newName string
	}
	byNewName := map[string][]indexed{}
	for _, r := range all {
		newName, err := normalization.CanonicalizeIndicatorName(ctx, indicatorAliases, r.IndicatorName)
		if err != nil {
			return rep, fmt.Errorf("canonicalize indicator name %q: %w", r.IndicatorName, err)
		}
		byNewName[newName] = append(byNewName[newName], indexed{row: r, newName: newName})
	}

	canonicalUnitFor := map[string]string{}
	for newName, group := range byNewName {
		sort.SliceStable(group, func(i, j int) bool {
			return takenAtLess(group[i].row.TakenAt, group[j].row.TakenAt)
		})
		for _, item := range group {
			if item.row.Unit != "" {
				canonicalUnitFor[newName] = item.row.Unit
				break
			}
		}
	}

	// Recompute NormalizedValue/NormalizedUnit for every row against its
	// group's canonical unit — same rule normalizeUnit uses internally:
	// exact match copies through, a recognized equivalence converts,
	// anything else is left zero rather than guessed.
	updated := map[string]normalization.LabResult{} // by row ID
	for newName, group := range byNewName {
		canonical, ok := canonicalUnitFor[newName]
		for _, item := range group {
			r := item.row
			r.IndicatorName = newName
			if ok && r.Unit != "" {
				if r.Unit == canonical {
					r.NormalizedValue, r.NormalizedUnit = r.Value, canonical
				} else if converted, convOK := normalization.ConvertUnit(r.Value, r.Unit, canonical); convOK {
					r.NormalizedValue, r.NormalizedUnit = converted, canonical
				} else {
					r.NormalizedValue, r.NormalizedUnit = 0, ""
				}
			}
			updated[r.ID] = r
		}
	}

	// Step 3: apply per-document, with intra-document exact-duplicate
	// removal.
	byDocument := map[string][]normalization.LabResult{}
	for _, r := range updated {
		byDocument[r.DocumentID] = append(byDocument[r.DocumentID], r)
	}
	for documentID, rows := range byDocument {
		sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

		kept, dropped, reviewGroups := dedupeDocument(rows)
		rep.duplicatesRemoved = append(rep.duplicatesRemoved, dropped...)
		rep.duplicatesForReview = append(rep.duplicatesForReview, reviewGroups...)

		changed := false
		for _, r := range kept {
			var original normalization.LabResult
			for _, o := range all {
				if o.ID == r.ID {
					original = o
					break
				}
			}
			if original.IndicatorName != r.IndicatorName {
				rep.renames = append(rep.renames, rename{userID: userID, documentID: documentID, rowID: r.ID, from: original.IndicatorName, to: r.IndicatorName})
			}
			if original.NormalizedValue != r.NormalizedValue || original.NormalizedUnit != r.NormalizedUnit {
				changed = true
			}
			if original.IndicatorName != r.IndicatorName {
				changed = true
			}
		}
		if len(dropped) > 0 {
			changed = true
		}

		if changed && apply {
			if err := labResults.ReplaceForDocument(ctx, documentID, kept); err != nil {
				return rep, fmt.Errorf("replace lab results for document %s: %w", documentID, err)
			}
		}
	}

	// Step 4: overwrite canonical_units for every group that has one.
	for newName, unit := range canonicalUnitFor {
		rep.canonicalUnitsSet = append(rep.canonicalUnitsSet, canonicalUnitChange{userID: userID, indicatorName: newName, unit: unit})
		if apply {
			if err := canonicalUnits.Set(ctx, userID, newName, unit); err != nil {
				return rep, fmt.Errorf("set canonical unit %s/%s: %w", userID, newName, err)
			}
		}
	}

	return rep, nil
}

// dedupeDocument splits rows (already all belonging to one document) into
// what to keep, what to unconditionally drop as a proven exact duplicate,
// and groups that look suspicious but are left alone for manual review —
// see this file's package doc comment, step 3, for the exact criterion.
func dedupeDocument(rows []normalization.LabResult) (kept []normalization.LabResult, dropped []normalization.LabResult, review []duplicateGroup) {
	type key struct {
		name  string
		val   float64
		unit  string
		at    int64
		hasAt bool
	}
	exact := map[key][]normalization.LabResult{}
	byName := map[string][]normalization.LabResult{}
	for _, r := range rows {
		byName[r.IndicatorName] = append(byName[r.IndicatorName], r)
		k := key{name: r.IndicatorName, val: r.Value, unit: r.Unit}
		if r.TakenAt != nil {
			k.at, k.hasAt = r.TakenAt.Unix(), true
		}
		exact[k] = append(exact[k], r)
	}

	toDrop := map[string]bool{}
	for _, group := range exact {
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].ID < group[j].ID })
		dropped = append(dropped, group[1:]...)
		for _, r := range group[1:] {
			toDrop[r.ID] = true
		}
	}

	for name, group := range byName {
		if len(group) < 2 {
			continue
		}
		var stillDistinct []normalization.LabResult
		for _, r := range group {
			if !toDrop[r.ID] {
				stillDistinct = append(stillDistinct, r)
			}
		}
		if len(stillDistinct) > 1 {
			review = append(review, duplicateGroup{indicatorName: name, rows: stillDistinct})
		}
	}

	for _, r := range rows {
		if !toDrop[r.ID] {
			kept = append(kept, r)
		}
	}
	return kept, dropped, review
}

// takenAtLess orders by TakenAt ascending with nil (undated) sorted last —
// see processUser's step 1+2 comment for why an undated row must never win
// the canonical-unit tie-break.
func takenAtLess(a, b *time.Time) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	return a.Before(*b)
}

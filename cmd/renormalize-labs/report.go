package main

import (
	"fmt"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

type rename struct {
	userID     string
	documentID string
	rowID      string
	from       string
	to         string
}

type canonicalUnitChange struct {
	userID        string
	indicatorName string
	unit          string
}

type duplicateGroup struct {
	indicatorName string
	rows          []normalization.LabResult
}

type report struct {
	renames             []rename
	duplicatesRemoved   []normalization.LabResult
	duplicatesForReview []duplicateGroup
	canonicalUnitsSet   []canonicalUnitChange
}

func (r *report) merge(other report) {
	r.renames = append(r.renames, other.renames...)
	r.duplicatesRemoved = append(r.duplicatesRemoved, other.duplicatesRemoved...)
	r.duplicatesForReview = append(r.duplicatesForReview, other.duplicatesForReview...)
	r.canonicalUnitsSet = append(r.canonicalUnitsSet, other.canonicalUnitsSet...)
}

func (r report) print(applied bool) {
	mode := "DRY RUN — no changes written"
	if applied {
		mode = "APPLIED"
	}
	fmt.Printf("=== renormalize-labs report (%s) ===\n\n", mode)

	fmt.Printf("Indicator renames: %d\n", len(r.renames))
	byFromTo := map[[2]string]int{}
	for _, rn := range r.renames {
		byFromTo[[2]string{rn.from, rn.to}]++
	}
	for pair, count := range byFromTo {
		fmt.Printf("  %-45s -> %-30s (%d row(s))\n", pair[0], pair[1], count)
	}

	fmt.Printf("\nCanonical units (re)established: %d\n", len(r.canonicalUnitsSet))
	for _, c := range r.canonicalUnitsSet {
		fmt.Printf("  %s / %-30s -> %s\n", c.userID, c.indicatorName, c.unit)
	}

	fmt.Printf("\nExact duplicates removed (same document, same indicator/value/unit/date): %d\n", len(r.duplicatesRemoved))
	for _, d := range r.duplicatesRemoved {
		fmt.Printf("  drop row %s: doc=%s indicator=%q value=%v unit=%q takenAt=%v\n", d.ID, d.DocumentID, d.IndicatorName, d.Value, d.Unit, d.TakenAt)
	}

	fmt.Printf("\nAmbiguous same-document/same-indicator groups left for manual review: %d\n", len(r.duplicatesForReview))
	for _, g := range r.duplicatesForReview {
		fmt.Printf("  indicator %q:\n", g.indicatorName)
		for _, row := range g.rows {
			fmt.Printf("    row %s: doc=%s value=%v unit=%q takenAt=%v\n", row.ID, row.DocumentID, row.Value, row.Unit, row.TakenAt)
		}
	}

	if !applied {
		fmt.Println("\nRe-run with --apply to write these changes. Back up the database first.")
	}
}

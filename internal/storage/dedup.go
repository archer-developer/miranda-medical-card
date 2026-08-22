package storage

import "strings"

// dedupKey canonicalizes free text — a drug name, a diagnosis name, a
// planned action's description — for same-source reprocess dedup:
// case/whitespace only, the same modest scope as internal/profile's
// groupKey. Real semantic matching (paraphrased descriptions, brand vs
// generic drug names) is a still-open problem, not attempted here — this is
// a cheap, simple guard against a fresh extraction of the same source
// literally re-describing something a surviving (protected) row already
// covers, not a general reconciliation mechanism.
func dedupKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

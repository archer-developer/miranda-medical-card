package ask

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDecodeKnowledgeRequest_ToDateCoversFullDay guards the fix for the
// midnight-To bug: time.Parse("2006-01-02", "2026-09-15") produces midnight
// (00:00:00 UTC), and storage uses date <= To.Unix(), which excluded every
// event logged after midnight on the same day. The fix moves To to
// end-of-day so self_reported_events (and any other date-filtered provider)
// returns all events on the requested date, not just those at exactly midnight.
func TestDecodeKnowledgeRequest_ToDateCoversFullDay(t *testing.T) {
	req, err := decodeKnowledgeRequest("self_reported_events", `{"from":"2026-09-15","to":"2026-09-15"}`)
	require.NoError(t, err)

	require.NotNil(t, req.From)
	require.NotNil(t, req.To)

	// From must be start-of-day (midnight).
	midnight, _ := time.Parse("2006-01-02", "2026-09-15")
	require.Equal(t, midnight.Unix(), req.From.Unix(), "From must be start of day")

	// To must cover the full day — any event at 23:59:59 must fall within it.
	lastSecond, _ := time.Parse("2006-01-02 15:04:05", "2026-09-15 23:59:59")
	require.True(t, req.To.After(lastSecond) || req.To.Equal(lastSecond),
		"To (%v) must be >= 23:59:59 on the same day so same-day events are not excluded", req.To)

	// To must not leak into the next day.
	nextDay, _ := time.Parse("2006-01-02", "2026-09-16")
	require.True(t, req.To.Before(nextDay),
		"To (%v) must not reach into the next calendar day", req.To)
}

// TestDecodeKnowledgeRequest_FromDateIsStartOfDay verifies that From is
// inclusive from midnight — no symmetric off-by-one on the other end.
func TestDecodeKnowledgeRequest_FromDateIsStartOfDay(t *testing.T) {
	req, err := decodeKnowledgeRequest("timeline", `{"from":"2026-09-15"}`)
	require.NoError(t, err)

	require.NotNil(t, req.From)
	midnight, _ := time.Parse("2006-01-02", "2026-09-15")
	require.Equal(t, midnight.Unix(), req.From.Unix())
}

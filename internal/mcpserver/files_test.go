package mcpserver

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestContentDispositionAttachment_NonASCIIFilename guards against the bug
// flagged in code review: fmt.Sprintf("...%q", filename) emits raw UTF-8
// bytes for non-ASCII filenames, which RFC 6266 doesn't sanction for the
// plain filename="..." parameter. This service's documents are
// Russian-language medical records, so Cyrillic filenames are the common
// case, not an edge case.
func TestContentDispositionAttachment_NonASCIIFilename(t *testing.T) {
	got := contentDispositionAttachment("анализ крови.pdf")

	require.Equal(t, `attachment; filename="______ _____.pdf"; filename*=UTF-8''%D0%B0%D0%BD%D0%B0%D0%BB%D0%B8%D0%B7%20%D0%BA%D1%80%D0%BE%D0%B2%D0%B8.pdf`, got)
}

func TestContentDispositionAttachment_ASCIIFilename(t *testing.T) {
	got := contentDispositionAttachment("cbc.pdf")

	require.Equal(t, `attachment; filename="cbc.pdf"; filename*=UTF-8''cbc.pdf`, got)
}

func TestAsciiFallbackFilename_EmptyInputFallsBackToFile(t *testing.T) {
	require.Equal(t, "file", asciiFallbackFilename(""))
}

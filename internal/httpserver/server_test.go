package httpserver_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/httpserver"
)

func TestNew_FilesRouteRequiresBearerToken(t *testing.T) {
	fileHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fileId=" + r.PathValue("fileId")))
	})
	mcpHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	linkHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := httpserver.New(mcpHandler, fileHandler, linkHandler, "secret-token")
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/files/file_123")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "no Authorization header must be rejected")
	_ = resp.Body.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/files/file_123", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "wrong token must be rejected")
	_ = resp.Body.Close()

	req, err = http.NewRequest(http.MethodGet, ts.URL+"/files/file_123", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestNew_LinksRouteDoesNotRequireBearerToken guards
// docs/adr/007-short-lived-file-links-for-profile-export.md's deliberate
// choice: /links/{linkId}'s access control is its random id + TTL, not the
// shared bearer token /mcp and /files require — a caller presenting no
// Authorization header at all must still reach the handler.
func TestNew_LinksRouteDoesNotRequireBearerToken(t *testing.T) {
	fileHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mcpHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	linkHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("linkId=" + r.PathValue("linkId")))
	})
	handler := httpserver.New(mcpHandler, fileHandler, linkHandler, "secret-token")
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/links/link_123")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "no Authorization header must still reach the link handler")
}

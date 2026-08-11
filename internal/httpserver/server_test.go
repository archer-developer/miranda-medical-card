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
	handler := httpserver.New(mcpHandler, fileHandler, "secret-token")
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

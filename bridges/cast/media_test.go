package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMediaHandler(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rain.mp3"), []byte("0123456789"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	srv := httptest.NewServer(newMediaHandler(dir))
	t.Cleanup(srv.Close)

	t.Run("serves a file", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/rain.mp3")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "bytes", resp.Header.Get("Accept-Ranges"))
	})

	t.Run("honours range requests", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/rain.mp3", nil)
		req.Header.Set("Range", "bytes=2-4")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
		assert.Equal(t, "bytes 2-4/10", resp.Header.Get("Content-Range"))
	})

	t.Run("no directory listing", func(t *testing.T) {
		for _, p := range []string{"/", "/sub/"} {
			resp, err := http.Get(srv.URL + p)
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusNotFound, resp.StatusCode, p)
		}
	})

	t.Run("dot files and dot directories are hidden", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".secret"), []byte("x"), 0o644))
		require.NoError(t, os.Mkdir(filepath.Join(dir, ".hidden"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".hidden", "a.mp3"), []byte("x"), 0o644))
		for _, p := range []string{"/.secret", "/.hidden/a.mp3"} {
			resp, err := http.Get(srv.URL + p)
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusNotFound, resp.StatusCode, p)
		}
	})

	t.Run("server sets a header timeout", func(t *testing.T) {
		assert.Equal(t, mediaReadHeaderTimeout, newMediaServer(dir).ReadHeaderTimeout)
	})

	t.Run("missing file", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/nope.mp3")
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("read only", func(t *testing.T) {
		before, err := os.ReadFile(filepath.Join(dir, "rain.mp3"))
		require.NoError(t, err)

		for _, method := range []string{
			http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
			http.MethodOptions, http.MethodTrace, "PROPFIND", "MKCOL",
		} {
			req, err := http.NewRequest(method, srv.URL+"/rain.mp3", strings.NewReader("overwrite"))
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err, method)
			resp.Body.Close()
			assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, method)
			assert.Equal(t, "GET, HEAD", resp.Header.Get("Allow"), method)
		}

		after, err := os.ReadFile(filepath.Join(dir, "rain.mp3"))
		require.NoError(t, err)
		assert.Equal(t, before, after, "a rejected write must leave the file untouched")

		_, err = os.Stat(filepath.Join(dir, "new.mp3"))
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("serves nothing outside the media files", func(t *testing.T) {
		// The media server shares no routes with the art proxy or anything else:
		// only files under dir are reachable.
		resp, err := http.Get(srv.URL + "/art/anything")
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

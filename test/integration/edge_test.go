//go:build integration

package integration_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Request limits and response hardening at the HTTP edge (review P0-4, P1-6, P1-7, P1-8).
func TestHTTPEdge(t *testing.T) {
	ts := testServer(t)
	u := registerIn(t, ts, "", "edge_"+uuid.NewString()[:8], "pw")
	room := createRoom(t, ts, u.token, "private")

	t.Run("oversized JSON body is 413", func(t *testing.T) {
		big := map[string]any{"body": strings.Repeat("x", 70<<10)} // limit is 64 KiB in testEnv
		assert.Equal(t, http.StatusRequestEntityTooLarge, status(t, apiPut(t, ts, "/rooms/"+room+"/send/m.text/big", big, u.token)))
		assert.Equal(t, http.StatusRequestEntityTooLarge, status(t, apiPost(t, ts, "/login", big, "")))
	})

	t.Run("event content must be a JSON object", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, status(t, apiPut(t, ts, "/rooms/"+room+"/send/m.text/arr", []int{1, 2}, u.token)))
		assert.Equal(t, http.StatusOK, status(t, apiPut(t, ts, "/rooms/"+room+"/send/m.text/obj", map[string]any{"body": "hi"}, u.token)))
	})

	t.Run("field limits", func(t *testing.T) {
		long := strings.Repeat("t", 256)
		assert.Equal(t, http.StatusBadRequest, status(t, apiPut(t, ts, "/rooms/"+room+"/send/m.text/"+long, map[string]any{}, u.token)))
		assert.Equal(t, http.StatusBadRequest, status(t, apiPost(t, ts, "/register", map[string]any{
			"username": "has space", "password": "pw", "device_name": "d"}, "")))
		assert.Equal(t, http.StatusBadRequest, status(t, apiPost(t, ts, "/rooms", map[string]any{"name": strings.Repeat("n", 300)}, u.token)))
	})

	t.Run("clients cannot self-declare a user type", func(t *testing.T) {
		for _, typ := range []string{"agent", "bot", "service", "system"} {
			assert.Equal(t, http.StatusBadRequest, status(t, apiPost(t, ts, "/register", map[string]any{
				"username": typ + "_" + uuid.NewString()[:8], "password": "pw", "device_name": "d", "user_type": typ}, "")), typ)
		}
	})

	t.Run("uploaded HTML is served inert", func(t *testing.T) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{`form-data; name="file"; filename="x.html"`}
		h["Content-Type"] = []string{"text/html"}
		part, err := mw.CreatePart(h)
		require.NoError(t, err)
		_, _ = part.Write([]byte("<script>alert(1)</script>"))
		require.NoError(t, mw.Close())
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/media/upload", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+u.token)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		mediaID := decodeJSON(t, resp)["media_id"].(string)

		dl := apiGet(t, ts, "/media/"+mediaID, u.token)
		defer dl.Body.Close()
		assert.Equal(t, "application/octet-stream", dl.Header.Get("Content-Type"))
		assert.Equal(t, "nosniff", dl.Header.Get("X-Content-Type-Options"))
		assert.Contains(t, dl.Header.Get("Content-Security-Policy"), "sandbox")
	})

	t.Run("media supports range requests", func(t *testing.T) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, _ := mw.CreateFormFile("file", "a.bin")
		_, _ = part.Write([]byte("0123456789"))
		_ = mw.Close()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/media/upload", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+u.token)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		mediaID := decodeJSON(t, resp)["media_id"].(string)

		get, _ := http.NewRequest(http.MethodGet, ts.URL+"/media/"+mediaID, nil)
		get.Header.Set("Authorization", "Bearer "+u.token)
		get.Header.Set("Range", "bytes=2-5")
		r2, err := http.DefaultClient.Do(get)
		require.NoError(t, err)
		defer r2.Body.Close()
		body, _ := io.ReadAll(r2.Body)
		assert.Equal(t, http.StatusPartialContent, r2.StatusCode)
		assert.Equal(t, "2345", string(body))
		assert.Equal(t, "nosniff", r2.Header.Get("X-Content-Type-Options"))
	})

	t.Run("upload over the media limit is 413", func(t *testing.T) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, _ := mw.CreateFormFile("file", "big.bin")
		_, _ = part.Write(bytes.Repeat([]byte{1}, 2<<20)) // limit is 1 MiB in testEnv
		_ = mw.Close()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/media/upload", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+u.token)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	})
}

// Brute force against one account is cut off even though each guess is a fresh request.
func TestRateLimits_LoginPerAccount(t *testing.T) {
	ts := testServer(t)
	u := registerIn(t, ts, "", "brute_"+uuid.NewString()[:8], "right-password")
	codes := make([]int, 0, 6)
	for range 6 {
		codes = append(codes, login(t, ts, "", u.name, "wrong-guess"))
	}
	assert.Equal(t, []int{401, 401, 401, 401, 401, 429}, codes)
	assert.Equal(t, http.StatusTooManyRequests, login(t, ts, "", u.name, "right-password"), "the account stays locked out until tokens refill")
}

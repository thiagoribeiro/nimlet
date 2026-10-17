package http

import (
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/thiagoribeiro/nimlet/internal/port/in"
)

func handleUploadMedia(mediaSvc in.MediaService) http.HandlerFunc {
	type response struct {
		ContentURI string `json:"content_uri"`
		MediaID    string `json:"media_id"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		actor := actorFrom(r.Context())
		// The server-wide ReadTimeout (15s) is sized for JSON; give uploads room. The size
		// is bounded separately by limitBody(MaxUploadBytes).
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(mediaTransferTimeout))

		// The outer Content-Type is "multipart/form-data; boundary=..." — that's the
		// envelope, not the uploaded file's type. Stream the "file" part out of it via
		// mime/multipart.Reader.NextPart, which reads incrementally and never buffers the
		// whole body, preserving the no-buffering intent of MediaService.Upload below.
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
			writeJSON(w, http.StatusBadRequest, apiError{Code: "BAD_REQUEST", Message: "expected multipart/form-data"})
			return
		}

		mr := multipart.NewReader(r.Body, params["boundary"])
		part, err := mr.NextPart()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, apiError{Code: "BAD_REQUEST", Message: "reading multipart body: " + err.Error()})
			return
		}
		defer part.Close()

		contentType := part.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}

		result, err := mediaSvc.Upload(r.Context(), actor.principal(), contentType, part)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, response{
			ContentURI: result.ContentURI,
			MediaID:    result.MediaID,
		})
	}
}

func handleDownloadMedia(mediaSvc in.MediaService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mediaID := chi.URLParam(r, "mediaId")

		rc, contentType, size, err := mediaSvc.Download(r.Context(), actorFrom(r.Context()).principal(), mediaID)
		if err != nil {
			writeError(w, r, err)
			return
		}
		defer rc.Close()
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(mediaTransferTimeout))

		// Uploaded bytes are untrusted and served from the API's origin: never let a browser
		// render or sniff them as active content.
		h := w.Header()
		h.Set("Content-Type", safeContentType(contentType))
		h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", mediaID))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
		h.Set("Cache-Control", "private, max-age=31536000, immutable") // media IDs never change content

		// A seekable body (the filesystem driver's *os.File) goes through ServeContent: Range
		// requests (resumable downloads, video seeking), HEAD, and net/http's sendfile path —
		// the bytes never enter user space. Other bodies (S3) are streamed without buffering.
		if rs, ok := rc.(io.ReadSeeker); ok {
			http.ServeContent(w, r, mediaID, time.Time{}, rs)
			return
		}
		h.Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, rc) // headers are sent: a mid-stream error can only end the response
	}
}

// mediaTransferTimeout bounds one upload or download, replacing the server's short
// JSON-sized read/write timeouts on the media routes.
const mediaTransferTimeout = 10 * time.Minute

// safeContentType serves types a browser could execute (HTML, SVG, XML, JavaScript) as
// opaque bytes; everything else keeps the type it was uploaded with.
func safeContentType(ct string) string {
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return "application/octet-stream"
	}
	switch {
	case strings.Contains(mediaType, "html"), strings.Contains(mediaType, "xml"),
		strings.Contains(mediaType, "javascript"), strings.Contains(mediaType, "ecmascript"),
		mediaType == "image/svg+xml":
		return "application/octet-stream"
	}
	return ct
}

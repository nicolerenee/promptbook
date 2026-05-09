package server_test

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// makeTestJPEG returns a tiny valid JPEG used as the upload payload.
// 4x4 px is enough for image.Decode to succeed.
func makeTestJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := range 4 {
		for y := range 4 {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}))
	return buf.Bytes()
}

// buildMultipart wraps a JPEG payload in a multipart/form-data body
// with field name "file" so the upload handlers see the same shape a
// browser would post.
func buildMultipart(t *testing.T, payload []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", "upload.jpg")
	require.NoError(t, err)
	_, err = fw.Write(payload)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return &buf, w.FormDataContentType()
}

// postMultipart drives an upload through the test server and returns
// the response status + decoded body.
func postMultipart(
	t *testing.T,
	srv *server.Server,
	path string,
	payload []byte,
) (int, map[string]any) {
	t.Helper()
	body, ct := buildMultipart(t, payload)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, path, body)
	req.Header.Set("Content-Type", ct)
	srv.Handler().ServeHTTP(rr, req)
	out := map[string]any{}
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
	}
	return rr.Code, out
}

// uploadTestServer builds the same fixture pickerTestServer does but
// returns the bare handles upload-path tests need. The DB is held in
// the test goroutine via t.Cleanup; tests don't need a handle to it
// today so we don't surface one.
func uploadTestServer(
	t *testing.T,
	recordingID, showID int64,
) (*server.Server, *imagecache.Cache) {
	t.Helper()
	ctx := t.Context()
	db, err := storage.Open(ctx, t.TempDir()+"/promptbook.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.ExecContext(ctx,
		`INSERT INTO shows (show_id, name) VALUES (?, ?)`, showID, "UploadShow")
	require.NoError(t, err)
	rawJSON, err := json.Marshal(map[string]any{
		"id": recordingID, "show": "UploadShow",
		"metadata": map[string]any{"show_id": showID},
	})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO recordings (recording_id, show_id, tour, date_full, raw_json)
		VALUES (?, ?, '', '', ?)
	`, recordingID, showID, string(rawJSON))
	require.NoError(t, err)

	cache := imagecache.New(t.TempDir(), nil, zerolog.Nop())
	srv, err := server.New(server.Options{DB: db, ImageCache: cache})
	require.NoError(t, err)
	return srv, cache
}

func TestAPIUploadBackdrop(t *testing.T) {
	t.Parallel()
	const recID, showID int64 = 71001, 5101
	srv, cache := uploadTestServer(t, recID, showID)

	status, body := postMultipart(t, srv,
		"/api/v1/recordings/"+strconv.FormatInt(recID, 10)+"/backdrop-upload",
		makeTestJPEG(t))

	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, true, body["ok"])
	idxF, ok := body["index"].(float64)
	require.True(t, ok, "index should be a number, got %T", body["index"])
	assert.GreaterOrEqual(t, int(idxF), imagecache.UploadIndexFloor,
		"uploaded backdrop should land at or above the upload floor")
	assert.True(t, cache.HasBackdrop(recID, int(idxF)),
		"backdrop file should exist at the returned index")
}

func TestAPIUploadShowPoster(t *testing.T) {
	t.Parallel()
	const recID, showID int64 = 71002, 5102
	srv, cache := uploadTestServer(t, recID, showID)

	status, body := postMultipart(t, srv,
		"/api/v1/shows/"+strconv.FormatInt(showID, 10)+"/poster-upload",
		makeTestJPEG(t))

	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, true, body["ok"])
	idxF, ok := body["index"].(float64)
	require.True(t, ok)
	assert.GreaterOrEqual(t, int(idxF), imagecache.UploadIndexFloor)
	assert.True(t, cache.HasPoster(showID, int(idxF)))
}

func TestAPIUploadRecordingPoster(t *testing.T) {
	t.Parallel()
	const recID, showID int64 = 71003, 5103
	srv, cache := uploadTestServer(t, recID, showID)

	status, body := postMultipart(t, srv,
		"/api/v1/recordings/"+strconv.FormatInt(recID, 10)+"/poster-upload",
		makeTestJPEG(t))

	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, true, body["ok"])
	idxF, ok := body["index"].(float64)
	require.True(t, ok)
	// Recording-as-show-poster must key the file on show_id, NOT
	// recording_id — that's the whole point of the wrapper.
	assert.True(t, cache.HasPoster(showID, int(idxF)),
		"recording-poster upload must land in the show's poster directory")
	assert.False(t, cache.HasPoster(recID, int(idxF)),
		"recording-poster upload must NOT use the recording id as key")
}

func TestAPIUploadRejectsBadImage(t *testing.T) {
	t.Parallel()
	const recID, showID int64 = 71004, 5104
	srv, _ := uploadTestServer(t, recID, showID)

	body, ct := buildMultipart(t, []byte("not an image at all"))
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/recordings/"+strconv.FormatInt(recID, 10)+"/backdrop-upload", body)
	req.Header.Set("Content-Type", ct)
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.Contains(t, strings.ToLower(rr.Body.String()), "decode")
}

func TestAPIUploadRequiresImageCache(t *testing.T) {
	t.Parallel()
	db, err := storage.Open(t.Context(), t.TempDir()+"/promptbook.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	tests := []struct {
		name string
		path string
	}{
		{name: "backdrop", path: "/api/v1/recordings/1/backdrop-upload"},
		{name: "show_poster", path: "/api/v1/shows/1/poster-upload"},
		{name: "recording_poster", path: "/api/v1/recordings/1/poster-upload"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status, _ := postMultipart(t, srv, tt.path, makeTestJPEG(t))
			assert.Equal(t, http.StatusServiceUnavailable, status)
		})
	}
}

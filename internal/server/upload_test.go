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
// returns the bare handles upload-path tests need.
func uploadTestServer(
	t *testing.T,
	recordingID, showID int64,
) (*server.Server, *imagecache.Cache) {
	t.Helper()
	ctx := t.Context()
	sqlDB, db, err := storage.OpenEnt(ctx, t.TempDir()+"/promptbook.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	require.NoError(t, db.Show.Create().SetID(showID).SetName("UploadShow").Exec(ctx))
	rawJSON, err := json.Marshal(map[string]any{
		"id": recordingID, "show": "UploadShow",
		"metadata": map[string]any{"show_id": showID},
	})
	require.NoError(t, err)
	require.NoError(t, db.Recording.Create().
		SetID(recordingID).SetShowID(showID).SetRawJSON(string(rawJSON)).Exec(ctx))

	cache := imagecache.New(t.TempDir(), nil, zerolog.Nop())
	srv, err := server.New(server.Options{DB: db, ImageCache: cache})
	require.NoError(t, err)
	return srv, cache
}

func TestAPIUploadRecordingFanart(t *testing.T) {
	t.Parallel()
	const recID, showID int64 = 71001, 5101
	srv, cache := uploadTestServer(t, recID, showID)

	status, body := postMultipart(t, srv,
		"/api/v1/recordings/"+strconv.FormatInt(recID, 10)+"/fanart-upload",
		makeTestJPEG(t))

	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, true, body["ok"])
	assert.True(t, cache.HasRecordingFanart(recID),
		"fanart.jpg should land at recordings/<id>/fanart.jpg")
}

func TestAPIUploadShowBanner(t *testing.T) {
	t.Parallel()
	const recID, showID int64 = 71002, 5102
	srv, cache := uploadTestServer(t, recID, showID)

	status, body := postMultipart(t, srv,
		"/api/v1/shows/"+strconv.FormatInt(showID, 10)+"/banner-upload",
		makeTestJPEG(t))

	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, true, body["ok"])
	assert.True(t, cache.HasShowBanner(showID))
}

func TestAPIUploadRecordingPosterTriggersRender(t *testing.T) {
	t.Parallel()
	const recID, showID int64 = 71003, 5103
	srv, cache := uploadTestServer(t, recID, showID)

	status, body := postMultipart(t, srv,
		"/api/v1/recordings/"+strconv.FormatInt(recID, 10)+"/poster-upload",
		makeTestJPEG(t))

	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, true, body["ok"])
	// poster-src.jpg lands; poster.jpg won't because the test server
	// has no Renderer wired (Options.ImageRenderer is nil), and the
	// regen path nil-checks.
	assert.True(t, cache.HasRecordingPosterSrc(recID),
		"poster-src.jpg should land after a successful upload")
}

func TestAPIUploadActorHeadshot(t *testing.T) {
	t.Parallel()
	srv, cache := uploadTestServer(t, 1, 1)

	const actorID int64 = 9001
	status, body := postMultipart(t, srv,
		"/api/v1/actors/"+strconv.FormatInt(actorID, 10)+"/headshot-upload",
		makeTestJPEG(t))

	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, true, body["ok"])
	assert.True(t, cache.HasHeadshot(actorID))
}

func TestAPIUploadRejectsBadImage(t *testing.T) {
	t.Parallel()
	const recID, showID int64 = 71004, 5104
	srv, _ := uploadTestServer(t, recID, showID)

	body, ct := buildMultipart(t, []byte("not an image at all"))
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/recordings/"+strconv.FormatInt(recID, 10)+"/fanart-upload", body)
	req.Header.Set("Content-Type", ct)
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.Contains(t, strings.ToLower(rr.Body.String()), "decode")
}

func TestAPIUploadRequiresImageCache(t *testing.T) {
	t.Parallel()
	sqlDB, db, err := storage.OpenEnt(t.Context(), t.TempDir()+"/promptbook.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	tests := []struct {
		name string
		path string
	}{
		{name: "fanart", path: "/api/v1/recordings/1/fanart-upload"},
		{name: "show_banner", path: "/api/v1/shows/1/banner-upload"},
		{name: "recording_poster", path: "/api/v1/recordings/1/poster-upload"},
		{name: "actor_headshot", path: "/api/v1/actors/1/headshot-upload"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status, _ := postMultipart(t, srv, tt.path, makeTestJPEG(t))
			assert.Equal(t, http.StatusServiceUnavailable, status)
		})
	}
}

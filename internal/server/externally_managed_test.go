package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// stubExternalEncoraClient implements ingest.Client without touching
// the network. Externally-managed imports never call Subtitles or
// AddToCollection; Recording is only consulted when the recording is
// not already in the local DB. The catalog-only test seeds the
// recording up front, so all three methods are no-ops.
type stubExternalEncoraClient struct{}

func (stubExternalEncoraClient) Recording(
	context.Context, int64,
) (encora.Recording, encora.RateLimitInfo, error) {
	return encora.Recording{}, encora.RateLimitInfo{Remaining: 30}, encora.ErrNotFound
}

func (stubExternalEncoraClient) Subtitles(
	context.Context, int64,
) ([]encora.Subtitle, encora.RateLimitInfo, error) {
	return nil, encora.RateLimitInfo{Remaining: 30}, nil
}

func (stubExternalEncoraClient) AddToCollection(
	context.Context, int64,
) (encora.RateLimitInfo, error) {
	return encora.RateLimitInfo{Remaining: 30}, nil
}

// stubExternalProber returns a benign 1080p MediaInfo without
// shelling out to ffprobe. Mirrors the ingest package's
// defaultStubProber but lives here so the server-test scope is
// hermetic.
type stubExternalProber struct{}

func (stubExternalProber) Probe(context.Context, string) (probe.MediaInfo, error) {
	return probe.MediaInfo{
		VideoCodec: "h264",
		Width:      1920,
		Height:     1080,
		Container:  "MP4",
	}, nil
}

// seedExternallyManagedRecording inserts the minimal Recording +
// Show rows the importQueueEntry resolver needs to load the target
// recording without consulting Encora. recordingID is hard-coded so
// the test can assert the post-import flag flip in the same row.
func seedExternallyManagedRecording(
	ctx context.Context, t *testing.T, db *ent.Client, recordingID int64,
) {
	t.Helper()
	require.NoError(t, db.Show.Create().SetID(7).SetName("Greenwich Beacon").Exec(ctx))
	rawJSON := `{"id":` + strconv.FormatInt(recordingID, 10) +
		`,"show":"Greenwich Beacon","tour":"Broadway",` +
		`"date":{"full_date":"2017-04-21","month_known":true,"day_known":true,"time":"evening"},` +
		`"master":"X","metadata":{"show_id":7}}`
	require.NoError(t, db.Recording.Create().
		SetID(recordingID).SetShowID(7).SetTour("Broadway").
		SetDateFull("2017-04-21").SetDateMonthKnown(true).SetDateDayKnown(true).
		SetMaster("X").
		SetRawJSON(rawJSON).
		Exec(ctx))
}

// TestGraphQLImportQueueEntryExternallyManaged exercises the full
// importQueueEntry mutation with externallyManaged=true wired against
// a real ingest.Engine: the source file MUST stay where it is, the
// .promptbook-externally-managed sentinel + .encora-id sidecar land
// in the source folder, recording_versions row points at the source
// path, and recordings.externally_managed flips to true.
func TestGraphQLImportQueueEntryExternallyManaged(t *testing.T) {
	t.Parallel()

	sqlDB, db, err := storage.OpenEnt(t.Context(),
		filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	const recordingID = int64(90100222)
	seedExternallyManagedRecording(t.Context(), t, db, recordingID)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "greenwich-beacon.mkv")
	srcBytes := []byte("video bytes — managed by Radarr, do not move")
	require.NoError(t, os.WriteFile(src, srcBytes, 0o600))

	libRoot := filepath.Join(t.TempDir(), "library")
	engine := &ingest.Engine{
		DB:             db,
		Client:         stubExternalEncoraClient{},
		LibraryRoot:    libRoot,
		FolderTemplate: "{Show} [encora-{EncoraID}]",
		FileTemplate:   "{Show}",
		Prober:         stubExternalProber{},
		Logger:         zerolog.New(io.Discard),
	}

	srv, err := server.New(server.Options{DB: db, IngestEngine: engine})
	require.NoError(t, err)

	suggested := recordingID
	queueID, err := storage.EnqueueFile(t.Context(), db, storage.QueueEntry{
		FilePath:             src,
		FileSizeBytes:        int64(len(srcBytes)),
		SuggestedRecordingID: &suggested,
		SuggestedConfidence:  storage.ConfidenceHigh,
	})
	require.NoError(t, err)

	mutation := `mutation Import($input: ImportQueueEntryInput!) {
		importQueueEntry(input: $input) { ok action error dest }
	}`
	body, rr := graphqlPostVars(t, srv.Handler(), mutation, map[string]any{
		"input": map[string]any{
			"queueID":           "queue-" + strconv.FormatInt(queueID, 10),
			"externallyManaged": true,
		},
	})
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			ImportQueueEntry struct {
				OK     bool   `json:"ok"`
				Action string `json:"action"`
				Error  string `json:"error"`
				Dest   string `json:"dest"`
			} `json:"importQueueEntry"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.True(t, resp.Data.ImportQueueEntry.OK,
		"externally-managed import must succeed: %s", string(body))
	assert.Equal(t, ingest.ActionMoved, resp.Data.ImportQueueEntry.Action)
	assert.Equal(t, src, resp.Data.ImportQueueEntry.Dest,
		"externally-managed dest must echo the source path")

	// Source file must still exist with its original bytes.
	got, err := os.ReadFile(src)
	require.NoError(t, err, "source file must still exist after import")
	assert.Equal(t, srcBytes, got, "source bytes must be untouched")

	// Library root stays empty — no canonical folder, no NFO.
	if entries, statErr := os.ReadDir(libRoot); statErr == nil {
		assert.Empty(t, entries,
			"library root must remain empty for externally-managed imports")
	}

	// Sentinel + .encora-id landed next to the source.
	_, err = os.Stat(filepath.Join(srcDir, ingest.ExternallyManagedSentinel))
	require.NoError(t, err, "sentinel must be present in source folder")
	idBytes, err := os.ReadFile(filepath.Join(srcDir, ".encora-id"))
	require.NoError(t, err)
	assert.Contains(t, string(idBytes), strconv.FormatInt(recordingID, 10))

	// Version row points at the source path verbatim.
	versions, err := storage.ListVersions(t.Context(), db, recordingID)
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, src, versions[0].FilePath)

	// Recording row's externally_managed flag is true.
	loaded, err := storage.LoadRecording(t.Context(), db, recordingID)
	require.NoError(t, err)
	assert.True(t, loaded.ExternallyManaged,
		"recording row's externally_managed flag must flip to true")

	// Queue row removed.
	_, err = storage.LoadQueueEntry(t.Context(), db, queueID)
	require.ErrorIs(t, err, storage.ErrQueueEntryNotFound)
}

// TestGraphQLSetRecordingExternallyManagedToggle exercises the
// setRecordingExternallyManaged mutation: flipping ON drops the
// sentinel + deletes any existing movie.nfo in the version's source
// folder; flipping OFF removes the sentinel. Files stay in place
// either way.
func TestGraphQLSetRecordingExternallyManagedToggle(t *testing.T) {
	t.Parallel()

	sqlDB, db, err := storage.OpenEnt(t.Context(),
		filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	const recordingID = int64(90100222)
	seedExternallyManagedRecording(t.Context(), t, db, recordingID)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "greenwich-beacon.mkv")
	require.NoError(t, os.WriteFile(src, []byte("video bytes"), 0o600))
	require.NoError(t, storage.UpsertVersion(t.Context(), db, storage.RecordingVersion{
		RecordingID: recordingID,
		FilePath:    src,
	}))

	nfoPath := filepath.Join(srcDir, "movie.nfo")
	require.NoError(t, os.WriteFile(nfoPath, []byte("<movie/>"), 0o600))

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	mutation := `mutation T($id: ID!, $value: Boolean!) {
		setRecordingExternallyManaged(recordingID: $id, externallyManaged: $value) {
			id
			externallyManaged
		}
	}`

	// Flip ON.
	body, rr := graphqlPostVars(t, srv.Handler(), mutation, map[string]any{
		"id":    "recording-" + strconv.FormatInt(recordingID, 10),
		"value": true,
	})
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	// Recording flag flipped + sentinel exists + movie.nfo gone.
	loaded, err := storage.LoadRecording(t.Context(), db, recordingID)
	require.NoError(t, err)
	assert.True(t, loaded.ExternallyManaged)
	_, err = os.Stat(filepath.Join(srcDir, ingest.ExternallyManagedSentinel))
	require.NoError(t, err, "sentinel must land in version source folder")
	_, err = os.Stat(nfoPath)
	require.True(t, os.IsNotExist(err),
		"existing movie.nfo must be deleted on flip-on")

	// Source file still exists.
	_, err = os.Stat(src)
	require.NoError(t, err, "source file must stay in place")

	// Flip OFF.
	body, rr = graphqlPostVars(t, srv.Handler(), mutation, map[string]any{
		"id":    "recording-" + strconv.FormatInt(recordingID, 10),
		"value": false,
	})
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	loaded, err = storage.LoadRecording(t.Context(), db, recordingID)
	require.NoError(t, err)
	assert.False(t, loaded.ExternallyManaged)
	_, err = os.Stat(filepath.Join(srcDir, ingest.ExternallyManagedSentinel))
	require.True(t, os.IsNotExist(err),
		"sentinel must be removed on flip-off")
	// Source file still exists.
	_, err = os.Stat(src)
	require.NoError(t, err, "source file must stay in place after flip-off")
}

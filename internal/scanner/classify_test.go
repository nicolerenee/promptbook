package scanner_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// classification mirrors the scanner.Classification wire shape so the
// tests can decode the queue row's classification_json blob without
// reaching for unexported types in the scanner package.
type classification struct {
	Parts       []classifiedFile     `json:"parts"`
	Extras      []classifiedFile     `json:"extras"`
	Ambiguous   bool                 `json:"ambiguous"`
	ExternalIDs []classifiedExternal `json:"externalIDs,omitempty"`
}

// classifiedExternal mirrors externalids.ExternalID's wire shape for
// the JSON-decoded fixture. Local to the tests so the scanner test
// package doesn't have to import externalids just to compare. Tag
// casing matches the externalids package's lowerCamelCase json tags.
type classifiedExternal struct {
	Provider   string `json:"provider"`
	ExternalID string `json:"externalID"`
	// RecordingID is omitempty on the wire — the scanner persists
	// pre-ingest entries with id=0 and the blob skips the field.
	RecordingID int64 `json:"recordingID,omitempty"`
}

type classifiedFile struct {
	Path          string `json:"path"`
	SizeBytes     int64  `json:"sizeBytes"`
	SuggestedKind string `json:"suggestedKind"`
	PartIndex     int    `json:"partIndex"`
}

// loadClassification scans, asserts a single queue row landed, and
// decodes its classification_json blob. Returns the classification +
// the queue entry so individual assertions can pin both shapes.
func (f *fixture) loadClassification(t *testing.T) (classification, storage.QueueEntry) {
	t.Helper()
	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	require.Len(t, got, 1)
	entry := got[0]
	require.NotEmpty(t, entry.ClassificationJSON,
		"folder-as-unit row must carry a classification_json blob")
	var cls classification
	require.NoError(t, json.Unmarshal([]byte(entry.ClassificationJSON), &cls))
	return cls, entry
}

// kindsByPath flattens a slice of classified files into a path → kind
// map for ergonomic assertions.
func kindsByPath(in []classifiedFile) map[string]string {
	out := make(map[string]string, len(in))
	for _, f := range in {
		out[f.Path] = f.SuggestedKind
	}
	return out
}

// TestClassifyMultipart covers rule 1: a folder with act-1 + act-2
// part markers produces ordered Parts (one per file) with no Extras
// and Ambiguous=false. The queue row's FilePath points at Parts[0]
// (the lower-indexed part) so legacy single-file consumers keep
// reading the right "main" path.
func TestClassifyMultipart(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Multipart Drop")
	act1 := filepath.Join(folder, "Some Show act-1.mkv")
	act2 := filepath.Join(folder, "Some Show act-2.mkv")
	writeFile(t, act1, 1024*1024)
	writeFile(t, act2, 1024*1024)

	cls, entry := f.loadClassification(t)

	assert.False(t, cls.Ambiguous)
	require.Len(t, cls.Parts, 2)
	assert.Empty(t, cls.Extras)
	assert.Equal(t, act1, cls.Parts[0].Path,
		"Parts must be ordered by index so part-1 is first")
	assert.Equal(t, "part-1", cls.Parts[0].SuggestedKind)
	assert.Equal(t, 1, cls.Parts[0].PartIndex)
	assert.Equal(t, act2, cls.Parts[1].Path)
	assert.Equal(t, "part-2", cls.Parts[1].SuggestedKind)
	assert.Equal(t, 2, cls.Parts[1].PartIndex)

	assert.Equal(t, act1, entry.FilePath,
		"queue row's FilePath must point at Parts[0] (the lowest-indexed part)")
	assert.Equal(t, 1, entry.ExtrasCount,
		"extras_count counts NON-main media files; act-2 reads as extra here")
}

// TestClassifyMainPlusFeaturette covers rule 2 + rule 5: one main
// video at the folder root + a sibling "bows" file. The bows file
// gets the featurette extras kind from the filename keyword scan.
func TestClassifyMainPlusFeaturette(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Greenwich Beacon Drop")
	main := filepath.Join(folder, "Greenwich Beacon - 2NT - 2022.07.24 M.mkv")
	bows := filepath.Join(folder, "bows! baker, burrage.mp4")
	writeFile(t, main, 8*1024*1024)
	writeFile(t, bows, 256*1024)

	cls, entry := f.loadClassification(t)

	assert.False(t, cls.Ambiguous)
	require.Len(t, cls.Parts, 1)
	assert.Equal(t, main, cls.Parts[0].Path)
	assert.Equal(t, "main", cls.Parts[0].SuggestedKind)

	require.Len(t, cls.Extras, 1)
	assert.Equal(t, bows, cls.Extras[0].Path)
	assert.Equal(t, "extra-featurette", cls.Extras[0].SuggestedKind,
		"top-level video with 'bows' in its name reads as extra-featurette")

	assert.Equal(t, main, entry.FilePath)
	assert.Equal(t, 1, entry.ExtrasCount)
}

// TestClassifyMainPlusAudioSubfolder covers rule 4: media files under
// audio/ pick up the extra-audio kind from the subfolder mapping. Each
// per-track rip becomes its own Extra.
func TestClassifyMainPlusAudioSubfolder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Halcyon Crossing Drop")
	main := filepath.Join(folder, "Halcyon Crossing.mkv")
	track1 := filepath.Join(folder, "audio", "01 - Wedding Song.mp3")
	track2 := filepath.Join(folder, "audio", "02 - Way Down Halcyon Crossing.mp3")
	writeFile(t, main, 4*1024*1024)
	writeFile(t, track1, 4*1024)
	writeFile(t, track2, 4*1024)

	cls, entry := f.loadClassification(t)

	assert.False(t, cls.Ambiguous)
	require.Len(t, cls.Parts, 1)
	assert.Equal(t, main, cls.Parts[0].Path)
	assert.Equal(t, "main", cls.Parts[0].SuggestedKind)

	require.Len(t, cls.Extras, 2)
	kinds := kindsByPath(cls.Extras)
	assert.Equal(t, "extra-audio", kinds[track1])
	assert.Equal(t, "extra-audio", kinds[track2])

	assert.Equal(t, main, entry.FilePath)
	assert.Equal(t, 2, entry.ExtrasCount)
}

// TestClassifyAmbiguousVideos covers rule 3: two similar-sized videos
// at folder root with no part markers. Ambiguous flips to true so the
// modal opens with the picker expanded; the largest is suggested as
// main and the other becomes an extra-other for the user to override.
func TestClassifyAmbiguousVideos(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Ambiguous Drop")
	candidateA := filepath.Join(folder, "Show - Cam A.mkv")
	candidateB := filepath.Join(folder, "Show - Cam B.mkv")
	// Within the 10% close band but distinguishable by a few bytes so
	// the largest tie-breaker is deterministic.
	writeFile(t, candidateA, 1024*1024)
	writeFile(t, candidateB, 1000*1024)

	cls, entry := f.loadClassification(t)

	assert.True(t, cls.Ambiguous,
		"two similar-sized root videos must trip the Ambiguous flag")
	require.Len(t, cls.Parts, 1)
	assert.Equal(t, candidateA, cls.Parts[0].Path,
		"largest of the candidates wins the suggested main slot")
	assert.Equal(t, "main", cls.Parts[0].SuggestedKind)

	require.Len(t, cls.Extras, 1)
	assert.Equal(t, candidateB, cls.Extras[0].Path)
	assert.Equal(t, "extra-other", cls.Extras[0].SuggestedKind,
		"sibling root video defaults to extra-other for user override")

	assert.Equal(t, candidateA, entry.FilePath)
	assert.Equal(t, 1, entry.ExtrasCount)
}

// TestClassifyEmptyFolder pins the contract that a folder with no
// media files (only photos / readmes / etc.) produces no queue row
// and no classification — preserves the legacy "skip empty" shape.
func TestClassifyEmptyFolder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Just Photos")
	writeFile(t, filepath.Join(folder, "poster.jpg"), 4*1024)
	writeFile(t, filepath.Join(folder, "notes.txt"), 32)

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Enqueued)
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	assert.Empty(t, got, "no media → no queue row, no classification")
}

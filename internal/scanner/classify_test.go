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
	Parts           []classifiedFile     `json:"parts"`
	Extras          []classifiedFile     `json:"extras"`
	Ambiguous       bool                 `json:"ambiguous"`
	ExternalIDs     []classifiedExternal `json:"externalIDs,omitempty"`
	DiscFormat      string               `json:"discFormat,omitempty"`
	DiscScaffolding []string             `json:"discScaffolding,omitempty"`
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

// TestClassifyIgnoresMetadataSidecars pins the re-scan-existing-
// library contract: when a folder already carries movie.nfo,
// poster.jpg, fanart.jpg etc. at the root (because promptbook or
// another tool wrote them on a prior pass), the scanner must NOT
// surface those files as importable extras. Subfolder content
// keeps its original treatment — a `photos/backdrop.jpg` inside
// the drop is real user content and lands in Extras.
func TestClassifyIgnoresMetadataSidecars(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Greenwich Beacon - existing library folder")
	main := filepath.Join(folder, "Greenwich Beacon.mkv")
	writeFile(t, main, 8*1024*1024)
	// Sidecars at the root: movie.nfo + poster + fanart. None of
	// these should land in Extras.
	writeFile(t, filepath.Join(folder, "movie.nfo"), 1024)
	writeFile(t, filepath.Join(folder, "poster.jpg"), 256*1024)
	writeFile(t, filepath.Join(folder, "fanart.jpg"), 512*1024)
	// Real user content in a subfolder named photos/ — gets the
	// extra-photo kind from the subfolder mapping.
	userPhoto := filepath.Join(folder, "photos", "curtain.jpg")
	writeFile(t, userPhoto, 2*1024)

	cls, entry := f.loadClassification(t)

	assert.False(t, cls.Ambiguous)
	require.Len(t, cls.Parts, 1)
	assert.Equal(t, main, cls.Parts[0].Path)

	require.Len(t, cls.Extras, 1,
		"sidecars at the folder root must be skipped; only the photos/ entry remains")
	assert.Equal(t, userPhoto, cls.Extras[0].Path)
	assert.Equal(t, 1, entry.ExtrasCount,
		"extras_count counts only the real user extra, not the sidecars")
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

// TestClassifyDVDFlatLayout covers the user's canonical case: a
// folder with VIDEO_TS.IFO + VTS_01_M.VOB chunks at the root (no
// nested VIDEO_TS/ subfolder). Content VOBs become Parts ordered by
// chunk M, ascending; scaffolding (.IFO / .BUP / menu VOBs) lives on
// DiscScaffolding; non-DVD extras flow through the regular
// classifyExtra pipeline. DiscFormat == "dvd".
func TestClassifyDVDFlatLayout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "9 to 5 (Closing Night)")
	// DVD scaffolding at the root.
	videoTSIFO := filepath.Join(folder, "VIDEO_TS.IFO")
	videoTSBUP := filepath.Join(folder, "VIDEO_TS.BUP")
	videoTSVOB := filepath.Join(folder, "VIDEO_TS.VOB")
	vts01IFO := filepath.Join(folder, "VTS_01_0.IFO")
	vts01BUP := filepath.Join(folder, "VTS_01_0.BUP")
	vts01MenuVOB := filepath.Join(folder, "VTS_01_0.VOB")
	writeFile(t, videoTSIFO, 8*1024)
	writeFile(t, videoTSBUP, 8*1024)
	writeFile(t, videoTSVOB, 16*1024)
	writeFile(t, vts01IFO, 8*1024)
	writeFile(t, vts01BUP, 8*1024)
	writeFile(t, vts01MenuVOB, 16*1024)
	// Five content chunks of ~1 GB each (we use small sizes for the
	// test — the scanner doesn't care about absolute size, only the
	// VTS_NN_M shape).
	vts1 := filepath.Join(folder, "VTS_01_1.VOB")
	vts2 := filepath.Join(folder, "VTS_01_2.VOB")
	vts3 := filepath.Join(folder, "VTS_01_3.VOB")
	vts4 := filepath.Join(folder, "VTS_01_4.VOB")
	vts5 := filepath.Join(folder, "VTS_01_5.VOB")
	writeFile(t, vts1, 1024*1024)
	writeFile(t, vts2, 1024*1024)
	writeFile(t, vts3, 1024*1024)
	writeFile(t, vts4, 1024*1024)
	writeFile(t, vts5, 512*1024)
	// Non-DVD extras: the original ripper's notes.
	rippersNotes := filepath.Join(folder, "info .txt")
	writeFile(t, rippersNotes, 256)

	cls, entry := f.loadClassification(t)

	assert.Equal(t, "dvd", cls.DiscFormat,
		"flat VIDEO_TS layout must classify as DVD")
	assert.False(t, cls.Ambiguous)
	require.Len(t, cls.Parts, 5,
		"five content VOBs (M >= 1) become Parts")
	// Parts ordered by integer chunk index ascending.
	assert.Equal(t, vts1, cls.Parts[0].Path)
	assert.Equal(t, 1, cls.Parts[0].PartIndex)
	assert.Equal(t, "part-1", cls.Parts[0].SuggestedKind)
	assert.Equal(t, vts2, cls.Parts[1].Path)
	assert.Equal(t, 2, cls.Parts[1].PartIndex)
	assert.Equal(t, vts3, cls.Parts[2].Path)
	assert.Equal(t, vts4, cls.Parts[3].Path)
	assert.Equal(t, vts5, cls.Parts[4].Path)
	assert.Equal(t, "part-5", cls.Parts[4].SuggestedKind)

	// Scaffolding: every .IFO / .BUP + menu VOBs (VIDEO_TS.VOB and
	// VTS_01_0.VOB). Sorted lexically for determinism.
	require.Len(t, cls.DiscScaffolding, 6,
		"two IFOs + two BUPs + two menu VOBs are scaffolding")
	scaffSet := map[string]bool{}
	for _, p := range cls.DiscScaffolding {
		scaffSet[p] = true
	}
	assert.True(t, scaffSet[videoTSIFO], "VIDEO_TS.IFO must be scaffolding")
	assert.True(t, scaffSet[videoTSBUP], "VIDEO_TS.BUP must be scaffolding")
	assert.True(t, scaffSet[videoTSVOB], "VIDEO_TS.VOB (disc menu) must be scaffolding")
	assert.True(t, scaffSet[vts01IFO], "VTS_01_0.IFO must be scaffolding")
	assert.True(t, scaffSet[vts01BUP], "VTS_01_0.BUP must be scaffolding")
	assert.True(t, scaffSet[vts01MenuVOB], "VTS_01_0.VOB (menu VOB) must be scaffolding")

	// Non-DVD extras: the ripper's notes file flows through the
	// regular extras pipeline.
	require.Len(t, cls.Extras, 1,
		"only the non-DVD info .txt lands in Extras")
	assert.Equal(t, rippersNotes, cls.Extras[0].Path,
		"info .txt is the only real extra")

	// Queue row points at the first content VOB; ExtrasCount counts
	// non-main media files (the other four content VOBs + the
	// non-DVD extra).
	assert.Equal(t, vts1, entry.FilePath,
		"FilePath = first content VOB")
}

// TestClassifyDVDNestedLayout covers the alternate disc layout where
// the rip preserves the original VIDEO_TS/ subfolder shape. Content
// VOBs still become Parts; scaffolding still rides on
// DiscScaffolding; the only difference is the source paths nest one
// level deeper. The mover (commit 2) flattens both layouts into the
// same canonical destination shape.
func TestClassifyDVDNestedLayout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Nested DVD Rip")
	videoTS := filepath.Join(folder, "VIDEO_TS")
	videoTSIFO := filepath.Join(videoTS, "VIDEO_TS.IFO")
	videoTSBUP := filepath.Join(videoTS, "VIDEO_TS.BUP")
	vts01IFO := filepath.Join(videoTS, "VTS_01_0.IFO")
	vts1 := filepath.Join(videoTS, "VTS_01_1.VOB")
	vts2 := filepath.Join(videoTS, "VTS_01_2.VOB")
	writeFile(t, videoTSIFO, 8*1024)
	writeFile(t, videoTSBUP, 8*1024)
	writeFile(t, vts01IFO, 8*1024)
	writeFile(t, vts1, 1024*1024)
	writeFile(t, vts2, 1024*1024)

	cls, _ := f.loadClassification(t)

	assert.Equal(t, "dvd", cls.DiscFormat,
		"nested VIDEO_TS/ layout must also classify as DVD")
	require.Len(t, cls.Parts, 2,
		"two content VOBs (M=1, M=2) become Parts")
	assert.Equal(t, vts1, cls.Parts[0].Path)
	assert.Equal(t, vts2, cls.Parts[1].Path)
	require.Len(t, cls.DiscScaffolding, 3,
		"two IFOs + one BUP under the nested VIDEO_TS/ are scaffolding")
	assert.Empty(t, cls.Extras,
		"no non-DVD files in this fixture")
}

// TestClassifyDVDMixedCaseFilenames pins the case-insensitivity
// contract: a real-world rip may have lowercase or mixed-case .vob
// / .ifo names. macOS-rip-on-Linux folders are a particularly
// frequent source of mixed casing.
func TestClassifyDVDMixedCaseFilenames(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Mixed Case DVD")
	writeFile(t, filepath.Join(folder, "Video_TS.ifo"), 8*1024)
	writeFile(t, filepath.Join(folder, "Video_TS.bup"), 8*1024)
	writeFile(t, filepath.Join(folder, "vts_01_0.ifo"), 8*1024)
	writeFile(t, filepath.Join(folder, "vts_01_0.bup"), 8*1024)
	vts1 := filepath.Join(folder, "vts_01_1.vob")
	vts2 := filepath.Join(folder, "Vts_01_2.VOB")
	writeFile(t, vts1, 1024*1024)
	writeFile(t, vts2, 1024*1024)

	cls, _ := f.loadClassification(t)

	assert.Equal(t, "dvd", cls.DiscFormat,
		"mixed-case Video_TS.ifo must still classify as DVD")
	require.Len(t, cls.Parts, 2,
		"both case variants of the content VOB must be detected")
	// Parts sorted by chunk integer, not lex string — even though in
	// this case the lexical and numeric orderings happen to agree.
	assert.Equal(t, vts1, cls.Parts[0].Path)
	assert.Equal(t, vts2, cls.Parts[1].Path)
}

// TestClassifyDVDDoubleDigitChunkOrder pins the integer-vs-lexical
// sort contract: a rip with M >= 10 (rare but legal — a single title
// set can hold up to 99 chunks per spec) must sort VTS_01_10.VOB
// AFTER VTS_01_9.VOB, not before. A naive sort.Strings would
// reverse them; classifyDVD sorts by integer chunk index instead.
func TestClassifyDVDDoubleDigitChunkOrder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Long DVD")
	writeFile(t, filepath.Join(folder, "VIDEO_TS.IFO"), 8*1024)
	writeFile(t, filepath.Join(folder, "VTS_01_0.IFO"), 8*1024)
	v1 := filepath.Join(folder, "VTS_01_1.VOB")
	v2 := filepath.Join(folder, "VTS_01_2.VOB")
	v9 := filepath.Join(folder, "VTS_01_9.VOB")
	v10 := filepath.Join(folder, "VTS_01_10.VOB")
	writeFile(t, v1, 1024*1024)
	writeFile(t, v2, 1024*1024)
	writeFile(t, v9, 1024*1024)
	writeFile(t, v10, 1024*1024)

	cls, _ := f.loadClassification(t)

	require.Len(t, cls.Parts, 4)
	assert.Equal(t, v1, cls.Parts[0].Path)
	assert.Equal(t, v2, cls.Parts[1].Path)
	assert.Equal(t, v9, cls.Parts[2].Path,
		"VTS_01_9.VOB must sort before VTS_01_10.VOB by chunk integer")
	assert.Equal(t, v10, cls.Parts[3].Path)
}

// TestClassifyDVDMultiScaffold covers a multi-act DVD layout where
// the show is split into two VIDEO_TS scaffolds at different
// directory roots (Act 1's VOBs at the folder root + Act 2's VOBs
// in a sibling subfolder). Each scaffold's VOBs use the same
// VTS_01_M chunk numbering, so a naive M-as-PartIndex assignment
// produces duplicate part indices and the multipart validator
// rejects the import. The classifier must renumber consecutively
// across scaffolds — Act 1's chunks stay parts 1..N, Act 2's
// chunks become parts (N+1)..(N+M).
func TestClassifyDVDMultiScaffold(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Waitress - 14 Jun 2016 - SJ Bernly")

	// Act 1 lives at the folder root.
	writeFile(t, filepath.Join(folder, "VIDEO_TS.IFO"), 8*1024)
	writeFile(t, filepath.Join(folder, "VTS_01_0.IFO"), 8*1024)
	a1v1 := filepath.Join(folder, "VTS_01_1.VOB")
	a1v2 := filepath.Join(folder, "VTS_01_2.VOB")
	writeFile(t, a1v1, 1024*1024)
	writeFile(t, a1v2, 1024*1024)

	// Act 2 lives in a sibling subfolder with its own scaffolding.
	act2 := filepath.Join(folder, "Act 2")
	writeFile(t, filepath.Join(act2, "VIDEO_TS.IFO"), 8*1024)
	writeFile(t, filepath.Join(act2, "VTS_01_0.IFO"), 8*1024)
	a2v1 := filepath.Join(act2, "VTS_01_1.VOB")
	a2v2 := filepath.Join(act2, "VTS_01_2.VOB")
	writeFile(t, a2v1, 1024*1024)
	writeFile(t, a2v2, 1024*1024)

	cls, _ := f.loadClassification(t)

	assert.Equal(t, "dvd", cls.DiscFormat,
		"multi-scaffold DVD must still classify as DVD")
	require.Len(t, cls.Parts, 4,
		"two acts × two content VOBs each = four parts total")
	// Act 1 (root scaffold, dir ".") comes first lexically before
	// the "Act 2" subfolder; within each scaffold, chunks ascend.
	assert.Equal(t, a1v1, cls.Parts[0].Path)
	assert.Equal(t, 1, cls.Parts[0].PartIndex,
		"Act 1 chunk 1 must be Part 1")
	assert.Equal(t, "part-1", cls.Parts[0].SuggestedKind)
	assert.Equal(t, a1v2, cls.Parts[1].Path)
	assert.Equal(t, 2, cls.Parts[1].PartIndex)
	assert.Equal(t, "part-2", cls.Parts[1].SuggestedKind)
	assert.Equal(t, a2v1, cls.Parts[2].Path)
	assert.Equal(t, 3, cls.Parts[2].PartIndex,
		"Act 2 chunk 1 must renumber to Part 3, not duplicate Part 1")
	assert.Equal(t, "part-3", cls.Parts[2].SuggestedKind)
	assert.Equal(t, a2v2, cls.Parts[3].Path)
	assert.Equal(t, 4, cls.Parts[3].PartIndex)
	assert.Equal(t, "part-4", cls.Parts[3].SuggestedKind)
}

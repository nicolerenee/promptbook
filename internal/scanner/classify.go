package scanner

import (
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nicolerenee/promptbook/internal/externalids"
	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/match"
)

// DiscFormatDVD is the DiscFormat token the classifier emits when a
// folder carries a VIDEO_TS DVD layout. Consumers (the ingest engine,
// the SPA's queue modal) check Classification.DiscFormat against this
// constant before applying the disc-aware mover branch. Empty
// DiscFormat preserves all legacy behaviour for non-disc folders.
//
// Re-exported from ingest.DiscFormatDVD so the scanner package owns
// the wire-format tokens its consumers (queue JSON, GraphQL) read,
// while the ingest engine still has a non-cyclic constant of its
// own — scanner already imports ingest for the assignment-kind
// tokens.
const DiscFormatDVD = ingest.DiscFormatDVD

// dvdVideoTSDirName is the canonical DVD title-set folder name. Some
// rips put the .IFO / .VOB scaffolding at the folder root; others nest
// it inside a VIDEO_TS/ subfolder. The classifier accepts either shape
// — match is case-insensitive so VIDEO_TS / Video_TS / video_ts all
// resolve.
const dvdVideoTSDirName = "VIDEO_TS"

// dvdContentVOBRE matches the standard DVD-spec content VOB filename
// shape: VTS_NN_M.VOB where NN is the title-set index (almost always
// 01) and M is the per-title-set chunk index. M == 0 is the title-set
// menu VOB (always small, never the recording itself); M >= 1 carries
// the actual content. Case-insensitive so real-world rips with mixed
// casing (vts_01_1.vob, Vts_01_1.VOB) all match.
//
// We compile once at package init so every folder scan reuses the same
// regexp instance — important for the polling scanner which can chew
// through hundreds of folders per pass on a watched-root scan.
var dvdContentVOBRE = regexp.MustCompile(`(?i)^VTS_(\d+)_(\d+)\.VOB$`)

// dvdScaffoldingExtSet is the lowercase-extension set the classifier
// recognizes as DVD scaffolding the ingest engine must move alongside
// the content VOBs (not as extras, not as part rows — verbatim
// pass-through into the canonical VIDEO_TS subfolder).
//
//nolint:gochecknoglobals // immutable lookup table.
var dvdScaffoldingExtSet = map[string]struct{}{
	".ifo": {},
	".bup": {},
}

// Classification is the per-folder role assignment the scanner emits
// for each folder-as-unit drop. Parts is the ordered list of files
// that together make up the recording (singleton for non-multipart
// drops). Extras is every other media file in the folder tree, each
// tagged with a heuristic-suggested kind. Ambiguous flags cases the
// modal needs to surface to the user — multiple similar-sized videos
// at the folder root with no part markers.
//
// ExternalIDs holds the third-party provider ids parsed from the
// folder basename (Radarr-style [tmdbid-N] / {imdb-ttN} markers; see
// ParseExternalIDsFromName for the supported tag families). The
// RecordingID field on each entry is 0 — the ingest engine stamps it
// once the recording is upserted. Empty slice for folders with no
// recognizable tag.
//
// DiscFormat is empty for the typical folder-as-unit drop. When the
// classifier detects a DVD layout (a VIDEO_TS.IFO at the folder root
// or inside a single nested VIDEO_TS/ subfolder), DiscFormat is set
// to DiscFormatDVD ("dvd") and the ingest engine takes the disc-
// preserving branch: content VOBs land inside a canonical VIDEO_TS/
// subfolder of the recording folder with their original DVD-spec
// names intact, and the .IFO / .BUP / menu VOB scaffolding follows
// verbatim so the media-server DVD playback engines (Plex / Emby /
// Jellyfin) can still parse the disc TOC.
//
// DiscScaffolding lists the absolute source paths of the DVD support
// files the engine must move alongside the content VOBs but should
// NOT track as version rows or surface as extras. Empty for non-disc
// folders; populated with every .IFO / .BUP / menu VOB inside the
// disc tree when DiscFormat == DiscFormatDVD.
type Classification struct {
	Parts           []ClassifiedFile         `json:"parts"`
	Extras          []ClassifiedFile         `json:"extras"`
	Ambiguous       bool                     `json:"ambiguous"`
	ExternalIDs     []externalids.ExternalID `json:"externalIDs,omitempty"`
	DiscFormat      string                   `json:"discFormat,omitempty"`
	DiscScaffolding []string                 `json:"discScaffolding,omitempty"`
}

// ClassifiedFile is one media file within a Classification. Path is
// the absolute path on disk; SuggestedKind is one of the assignment
// kind tokens from internal/ingest (main / part-N / extra-{kind}).
// PartIndex is 0 for non-parts and >0 for ordered parts.
type ClassifiedFile struct {
	Path          string `json:"path"`
	SizeBytes     int64  `json:"sizeBytes"`
	SuggestedKind string `json:"suggestedKind"`
	PartIndex     int    `json:"partIndex"`
}

// subfolderKindKeywords maps lowercase subfolder-name fragments to the
// extras kind they imply when a media file lives under that subfolder.
// Keys are matched as substrings of the lowercased subfolder name so
// "Behind The Scenes" and "behindthescenes" both land on the right
// kind. Order matters when keys overlap; longer / more specific keys
// MUST come first so e.g. "behindthescenes" wins over a shorter "bts"
// substring inside an unrelated path component.
//
//nolint:gochecknoglobals // immutable lookup table.
var subfolderKindKeywords = []struct {
	keyword string
	kind    string
}{
	{"behindthescenes", ingest.ExtraKindBehindTheScenes},
	{"behind the scenes", ingest.ExtraKindBehindTheScenes},
	{"behind-the-scenes", ingest.ExtraKindBehindTheScenes},
	{"featurettes", ingest.ExtraKindFeaturette},
	{"featurette", ingest.ExtraKindFeaturette},
	{"interviews", ingest.ExtraKindInterview},
	{"interview", ingest.ExtraKindInterview},
	{"trailers", ingest.ExtraKindTrailer},
	{"trailer", ingest.ExtraKindTrailer},
	{"deletedscenes", ingest.ExtraKindDeletedScenes},
	{"deleted scenes", ingest.ExtraKindDeletedScenes},
	{"scenes", ingest.ExtraKindScene},
	{"pictures", ingest.ExtraKindPhoto},
	{"photos", ingest.ExtraKindPhoto},
	{"audio", ingest.ExtraKindAudio},
	{"bts", ingest.ExtraKindBehindTheScenes},
	{"bonus", ingest.ExtraKindFeaturette},
	{"extras", ingest.ExtraKindFeaturette},
}

// topLevelKindKeywords maps lowercase substrings of a top-level
// extras filename to the extras kind they imply. Same matching rules
// as subfolderKindKeywords — substring against the lowercased
// basename, longer keys first.
//
//nolint:gochecknoglobals // immutable lookup table.
var topLevelKindKeywords = []struct {
	keyword string
	kind    string
}{
	{"behind the scenes", ingest.ExtraKindBehindTheScenes},
	{"behindthescenes", ingest.ExtraKindBehindTheScenes},
	{"rehearsal", ingest.ExtraKindBehindTheScenes},
	{"interview", ingest.ExtraKindInterview},
	{"trailer", ingest.ExtraKindTrailer},
	{"commercial", ingest.ExtraKindTrailer},
	{"promo", ingest.ExtraKindTrailer},
	{"curtain", ingest.ExtraKindFeaturette},
	{"applause", ingest.ExtraKindFeaturette},
	{"bows", ingest.ExtraKindFeaturette},
	{"bts", ingest.ExtraKindBehindTheScenes},
}

// classifyFolder produces a Classification for a folder-as-unit. The
// rules (in the order applied):
//
//  0. If the folder carries a DVD layout (VIDEO_TS.IFO at the root or
//     in a single nested VIDEO_TS/ subfolder), take the DVD branch:
//     content VOBs become Parts (one per VTS_NN_M.VOB where M>=1,
//     ordered by integer M); .IFO / .BUP / menu VOBs go onto
//     DiscScaffolding; non-DVD files (info .txt, etc.) flow through
//     the regular extras pipeline.
//  1. If any file's basename carries a part marker (act 1 / pt-2 /
//     part 3) per match.Parse, those files become Parts ordered by
//     index; everything else is an Extra.
//  2. Otherwise, exactly one root-level video → that's the main; rest
//     are Extras.
//  3. Otherwise, multiple root-level videos within 10% size of each
//     other → Ambiguous; suggest the largest as main, others as
//     extra-other. Modal will surface for the user to override.
//  4. Files in subdirectories always become Extras with a kind read
//     from the deepest subfolder name (audio → audio, photos →
//     photo, behindthescenes → behindthescenes, …).
//  5. Top-level video extras (case 2's "rest" or case 3's losers) get
//     a kind from filename keyword match (bows → featurette, etc.).
//
// folder is the absolute path of the folder being classified.
func classifyFolder(folder string, media []mediaFile) Classification {
	cls := Classification{Parts: []ClassifiedFile{}, Extras: []ClassifiedFile{}}
	if len(media) == 0 {
		return cls
	}

	if dvd, ok := classifyDVD(folder, media); ok {
		return dvd
	}

	// Split into eligible-main candidates (video + audio) and always-
	// extras (images, subtitles, documents, anything else). The
	// classification heuristic only runs over the eligible set; the
	// always-extras list gets concatenated onto Extras at the end so
	// every file in the folder is preserved in the queue row.
	candidates, alwaysExtras := splitCandidates(media)
	if len(candidates) == 0 {
		// No video / audio in the folder — no recording to anchor an
		// import on. Skip rather than enqueue a photo-only folder.
		return cls
	}

	parts := detectParts(candidates)
	if len(parts) > 0 {
		cls.Parts = parts
		// Everything not in parts (other media + always-extras) becomes
		// an extra.
		partPaths := make(map[string]bool, len(parts))
		for _, p := range parts {
			partPaths[p.Path] = true
		}
		for _, m := range candidates {
			if partPaths[m.path] {
				continue
			}
			cls.Extras = append(cls.Extras, classifyExtra(folder, m))
		}
		for _, m := range alwaysExtras {
			cls.Extras = append(cls.Extras, classifyExtra(folder, m))
		}
		return cls
	}

	// No part markers — split candidates into root-level videos + rest.
	rootVideos, others := splitRootVideos(candidates)

	switch len(rootVideos) {
	case 0:
		// No top-level videos. Fall back to the legacy mainFile
		// heuristic: largest candidate wins, with a root-over-nested
		// tie-breaker inside the 10% close band so a tiny teaser.mp3
		// at the root still beats a slightly-bigger track in audio/.
		main, rest := pickLargestWithRootTiebreak(candidates)
		cls.Parts = []ClassifiedFile{{
			Path:          main.path,
			SizeBytes:     main.size,
			SuggestedKind: ingest.AssignmentKindMain,
		}}
		for _, m := range rest {
			cls.Extras = append(cls.Extras, classifyExtra(folder, m))
		}
	case 1:
		// Exactly one top-level video — that's the main; everything
		// else is an extra.
		cls.Parts = []ClassifiedFile{{
			Path:          rootVideos[0].path,
			SizeBytes:     rootVideos[0].size,
			SuggestedKind: ingest.AssignmentKindMain,
		}}
		for _, m := range others {
			cls.Extras = append(cls.Extras, classifyExtra(folder, m))
		}
	default:
		cls = classifyAmbiguousRootVideos(folder, rootVideos, others)
	}
	for _, m := range alwaysExtras {
		cls.Extras = append(cls.Extras, classifyExtra(folder, m))
	}
	return cls
}

// splitCandidates partitions media into "eligible main candidates"
// (video + audio) and "always extras" (everything else — images,
// subtitles, documents, …). Used by classifyFolder so the heuristic
// only runs over files that could actually BE the recording, while
// non-media files still get tracked + moved alongside.
func splitCandidates(media []mediaFile) ([]mediaFile, []mediaFile) {
	var candidates, others []mediaFile
	for _, m := range media {
		if m.isVideo || m.isAudio {
			candidates = append(candidates, m)
			continue
		}
		others = append(others, m)
	}
	return candidates, others
}

// classifyAmbiguousRootVideos handles the "multiple top-level videos,
// no part markers" branch. The largest is suggested as main; any
// sibling within the 10% close band raises Ambiguous so the modal
// surfaces a picker. Decisively-smaller siblings get their kind from
// the top-level filename keyword scan (bows → featurette,
// trailer → trailer, …) — only the close-band case defaults to
// extra-other so the user can override in the modal.
func classifyAmbiguousRootVideos(
	folder string, rootVideos, others []mediaFile,
) Classification {
	cls := Classification{Parts: []ClassifiedFile{}, Extras: []ClassifiedFile{}}

	main, rest := pickLargest(rootVideos)
	cls.Parts = []ClassifiedFile{{
		Path:          main.path,
		SizeBytes:     main.size,
		SuggestedKind: ingest.AssignmentKindMain,
	}}
	const percentDenom = 100
	threshold := main.size - main.size*closeBandPct/percentDenom
	for _, m := range rest {
		kind := kindFromFilename(filepath.Base(m.path))
		if m.size >= threshold {
			// Within the 10% close band — the heuristic can't decide
			// confidently between main and a sibling. Force extra-other
			// so the modal opens with the picker expanded and the user
			// re-classifies.
			cls.Ambiguous = true
			kind = ingest.ExtraKindOther
		}
		cls.Extras = append(cls.Extras, ClassifiedFile{
			Path:          m.path,
			SizeBytes:     m.size,
			SuggestedKind: ingest.AssignmentKindExtra(kind),
		})
	}
	for _, m := range others {
		cls.Extras = append(cls.Extras, classifyExtra(folder, m))
	}
	return cls
}

// minPartsForMultipart is the lower bound for declaring a folder
// multipart. A single "act 1" file alone isn't multipart; it's a
// misnamed loose recording the user can re-classify in the modal.
const minPartsForMultipart = 2

// detectParts returns ordered Parts when at least minPartsForMultipart
// media files carry part markers in their basenames. Returns nil
// otherwise so the caller falls through to the single-main /
// ambiguous branches.
func detectParts(media []mediaFile) []ClassifiedFile {
	type withIndex struct {
		file  mediaFile
		index int
	}
	var hits []withIndex
	for _, m := range media {
		parsed := match.Parse(filepath.Base(m.path))
		if parsed.PartIndex > 0 {
			hits = append(hits, withIndex{file: m, index: parsed.PartIndex})
		}
	}
	if len(hits) < minPartsForMultipart {
		return nil
	}
	sort.SliceStable(hits, func(i, j int) bool {
		return hits[i].index < hits[j].index
	})
	out := make([]ClassifiedFile, 0, len(hits))
	for _, h := range hits {
		out = append(out, ClassifiedFile{
			Path:          h.file.path,
			SizeBytes:     h.file.size,
			SuggestedKind: ingest.AssignmentKindPart(h.index),
			PartIndex:     h.index,
		})
	}
	return out
}

// splitRootVideos partitions media into "video files at the folder
// root" and "everything else" (audio, nested files of any kind). Used
// to drive the case-2 / case-3 branch decision.
func splitRootVideos(media []mediaFile) ([]mediaFile, []mediaFile) {
	var rootVideos, rest []mediaFile
	for _, m := range media {
		if m.rootLevel && m.isVideo {
			rootVideos = append(rootVideos, m)
			continue
		}
		rest = append(rest, m)
	}
	return rootVideos, rest
}

// pickLargest returns the largest entry by byte size and the rest of
// the slice (in input order minus the leader). Caller guarantees a
// non-empty slice.
func pickLargest(in []mediaFile) (mediaFile, []mediaFile) {
	leader := in[0]
	for _, m := range in[1:] {
		if m.size > leader.size {
			leader = m
		}
	}
	rest := make([]mediaFile, 0, len(in)-1)
	for _, m := range in {
		if m.path == leader.path {
			continue
		}
		rest = append(rest, m)
	}
	return leader, rest
}

// pickLargestWithRootTiebreak preserves the legacy mainFile heuristic
// for the "no top-level videos" branch of classifyFolder: largest by
// size first, then a root-level / video-preferred tie-breaker for any
// candidate within the 10% close band of the leader. The 10% band is
// wide enough to forgive container-overhead differences without
// letting a single per-track audio rip masquerade as the main file.
//
// Returns the leader plus the rest of the slice in input order minus
// the leader. Caller guarantees a non-empty slice.
func pickLargestWithRootTiebreak(in []mediaFile) (mediaFile, []mediaFile) {
	leader, _ := pickLargest(in)
	const percentDenom = 100
	threshold := leader.size - leader.size*closeBandPct/percentDenom
	for _, m := range in {
		if m.path == leader.path {
			continue
		}
		if m.size < threshold {
			continue
		}
		if betterTiebreakMain(m, leader) {
			leader = m
		}
	}
	rest := make([]mediaFile, 0, len(in)-1)
	for _, m := range in {
		if m.path == leader.path {
			continue
		}
		rest = append(rest, m)
	}
	return leader, rest
}

// betterTiebreakMain returns true when candidate beats current as the
// main file under the close-size tie-breaker rules: prefer a root-
// level file over a nested one, then prefer a video over an audio
// file when the root-level state is equal.
func betterTiebreakMain(candidate, current mediaFile) bool {
	if candidate.rootLevel && !current.rootLevel {
		return true
	}
	if !candidate.rootLevel && current.rootLevel {
		return false
	}
	if candidate.isVideo && !current.isVideo {
		return true
	}
	return false
}

// closeBandPct is the percentage tolerance used by the ambiguous-
// videos heuristic. Two top-level videos sized within this band are
// "similar enough" that the scanner can't confidently pick one as
// main; the modal must prompt.
const closeBandPct = 10

// classifyExtra returns the ClassifiedFile for one extras-bucket
// hit. Kind precedence (most specific wins):
//  1. Subfolder keyword (audio/, photos/, behindthescenes/, …).
//  2. File extension class (image → photo, anything else falls
//     through to step 3).
//  3. Filename keyword scan (bows → featurette, trailer, …).
//  4. ExtraKindOther fallback.
//
// The extension step is what lets a top-level `.jpg` get tagged as
// extra-photo without a matching subfolder name; without it the
// filename-keyword scan would default to extra-other for every
// image at the folder root.
func classifyExtra(folder string, m mediaFile) ClassifiedFile {
	out := ClassifiedFile{
		Path:      m.path,
		SizeBytes: m.size,
	}
	rel := relativePath(folder, m.path)
	dir := filepath.Dir(rel)
	if dir != "." && dir != "" {
		out.SuggestedKind = ingest.AssignmentKindExtra(kindFromSubfolder(dir))
		return out
	}
	if k := kindFromExtension(filepath.Ext(m.path)); k != "" {
		out.SuggestedKind = ingest.AssignmentKindExtra(k)
		return out
	}
	out.SuggestedKind = ingest.AssignmentKindExtra(kindFromFilename(filepath.Base(m.path)))
	return out
}

// kindFromExtension maps a file extension to an extras kind. Audio
// extensions are intentionally excluded — those are eligible main
// candidates and should never reach this code path through the
// classifier; they're listed here only as a safety net for
// hand-built mediaFile slices in tests. Returns "" when the
// extension doesn't have a typed mapping so the caller falls
// through to the filename-keyword scan.
func kindFromExtension(ext string) string {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".tiff", ".tif":
		return ingest.ExtraKindPhoto
	case ".mp3", ".flac", ".wav", ".m4a", ".aac", ".ogg", ".opus":
		return ingest.ExtraKindAudio
	}
	return ""
}

// relativePath returns m relative to folder using filepath.Rel.
// Returns the basename when filepath.Rel fails (defensive — the
// folder walker only emits paths under folder, so this should not
// fire in practice).
func relativePath(folder, path string) string {
	rel, err := filepath.Rel(folder, path)
	if err != nil {
		return filepath.Base(path)
	}
	return rel
}

// kindFromSubfolder picks an extras kind for a file living under a
// subdirectory whose name (or any ancestor segment) matches one of
// the subfolderKindKeywords. Returns ExtraKindOther when no keyword
// matches.
func kindFromSubfolder(dir string) string {
	lc := strings.ToLower(dir)
	for _, m := range subfolderKindKeywords {
		if strings.Contains(lc, m.keyword) {
			return m.kind
		}
	}
	return ingest.ExtraKindOther
}

// kindFromFilename picks an extras kind for a top-level video file
// based on filename keywords (bows / interview / trailer / …).
// Returns ExtraKindOther when no keyword matches.
func kindFromFilename(name string) string {
	lc := strings.ToLower(name)
	for _, m := range topLevelKindKeywords {
		if strings.Contains(lc, m.keyword) {
			return m.kind
		}
	}
	return ingest.ExtraKindOther
}

// classifyDVD inspects media for a DVD layout. Returns (cls, true)
// when the folder carries VIDEO_TS scaffolding the engine must
// preserve verbatim; (zero, false) otherwise so the caller falls
// through to the regular classification heuristics.
//
// Detection rule: at least one VIDEO_TS.IFO somewhere in the folder
// tree. Real-world rips put the .IFO scaffolding either at the
// folder root (the "flat" shape — the user's 9 to 5 example) or
// inside a single nested VIDEO_TS/ subfolder. Both shapes produce
// the same Classification — the ingest engine's mover handles the
// flattening into the canonical VIDEO_TS/ destination subfolder
// regardless of source layout.
//
// Output shape when DVD is detected:
//   - Parts: every VTS_NN_M.VOB where M>=1, ordered by integer M
//     (so VTS_01_10.VOB sorts after VTS_01_9.VOB rather than
//     lexically before it). Each carries SuggestedKind
//     "part-{M}" + PartIndex M so the ingest engine's existing
//     multipart path handles them without special casing.
//   - DiscScaffolding: absolute paths of every .IFO, .BUP, and the
//     menu VOB siblings (VIDEO_TS.VOB + VTS_NN_0.VOB). These are
//     invisible to the queue modal's extras picker but the mover
//     carries them into the destination so DVD playback works.
//   - Extras: every non-DVD-related file the folder contains
//     (info .txt, the ripper's notes, any user-added bonus
//     material). Run through the regular classifyExtra pipeline
//     so the kind/subfolder mapping still applies.
//   - DiscFormat: DiscFormatDVD.
func classifyDVD(folder string, media []mediaFile) (Classification, bool) {
	if !looksLikeDVD(media) {
		return Classification{}, false
	}
	cls := Classification{
		Parts:      []ClassifiedFile{},
		Extras:     []ClassifiedFile{},
		DiscFormat: DiscFormatDVD,
	}
	type partWithIndex struct {
		file  ClassifiedFile
		chunk int
	}
	var parts []partWithIndex
	var scaffolding []string
	for _, m := range media {
		base := filepath.Base(m.path)
		ext := strings.ToLower(filepath.Ext(base))
		if _, isScaffoldExt := dvdScaffoldingExtSet[ext]; isScaffoldExt {
			scaffolding = append(scaffolding, m.path)
			continue
		}
		if matches := dvdContentVOBRE.FindStringSubmatch(base); matches != nil {
			// Group 2 is the chunk index M; the regexp already
			// constrained both groups to digits, so Atoi cannot fail.
			chunk, _ := strconv.Atoi(matches[2])
			if chunk == 0 {
				// Menu VOB — scaffolding, not content.
				scaffolding = append(scaffolding, m.path)
				continue
			}
			parts = append(parts, partWithIndex{
				file: ClassifiedFile{
					Path:          m.path,
					SizeBytes:     m.size,
					SuggestedKind: ingest.AssignmentKindPart(chunk),
					PartIndex:     chunk,
				},
				chunk: chunk,
			})
			continue
		}
		if strings.EqualFold(base, "VIDEO_TS.VOB") {
			// VIDEO_TS.VOB is the disc-menu VOB — scaffolding even
			// though it doesn't match the VTS_NN_M shape.
			scaffolding = append(scaffolding, m.path)
			continue
		}
		// Non-DVD file inside the disc tree (info .txt, etc.). Flow
		// through the regular extras pipeline so the kind heuristic
		// still applies. Skipping files that live inside the nested
		// VIDEO_TS/ subfolder is intentional — those should never be
		// surfaced as extras (the user didn't put them there).
		if insideVideoTSSubfolder(folder, m.path) {
			scaffolding = append(scaffolding, m.path)
			continue
		}
		cls.Extras = append(cls.Extras, classifyExtra(folder, m))
	}
	sort.SliceStable(parts, func(i, j int) bool {
		return parts[i].chunk < parts[j].chunk
	})
	for _, p := range parts {
		cls.Parts = append(cls.Parts, p.file)
	}
	// Sort scaffolding by path so the destination order is stable
	// across scans. The ingest engine doesn't care about order — it
	// moves every entry verbatim — but tests + diffs read better with
	// a deterministic shape.
	sort.Strings(scaffolding)
	cls.DiscScaffolding = scaffolding
	return cls, true
}

// looksLikeDVD reports whether media contains at least one
// VIDEO_TS.IFO file. Case-insensitive so real-world rips with mixed
// casing (Video_TS.ifo, VIDEO_ts.IFO) all match. Nested layouts
// (VIDEO_TS/VIDEO_TS.IFO) and flat layouts (VIDEO_TS.IFO at the
// folder root) both qualify because we only care that the disc-TOC
// file is present somewhere — the mover handles the source-vs-dest
// layout difference.
func looksLikeDVD(media []mediaFile) bool {
	for _, m := range media {
		if strings.EqualFold(filepath.Base(m.path), "VIDEO_TS.IFO") {
			return true
		}
	}
	return false
}

// insideVideoTSSubfolder reports whether path lives under a nested
// VIDEO_TS/ subfolder of folder. Used by classifyDVD to keep any
// stray file inside that subtree out of the extras list — every
// non-DVD file under VIDEO_TS/ moves to the destination's
// VIDEO_TS/ verbatim alongside the scaffolding so the source layout
// rebuilds cleanly. Case-insensitive on the path components.
func insideVideoTSSubfolder(folder, path string) bool {
	rel, err := filepath.Rel(folder, path)
	if err != nil {
		return false
	}
	for segment := range strings.SplitSeq(filepath.Dir(rel), string(filepath.Separator)) {
		if strings.EqualFold(segment, dvdVideoTSDirName) {
			return true
		}
	}
	return false
}

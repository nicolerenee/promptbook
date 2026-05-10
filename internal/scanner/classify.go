package scanner

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/match"
)

// Classification is the per-folder role assignment the scanner emits
// for each folder-as-unit drop. Parts is the ordered list of files
// that together make up the recording (singleton for non-multipart
// drops). Extras is every other media file in the folder tree, each
// tagged with a heuristic-suggested kind. Ambiguous flags cases the
// modal needs to surface to the user — multiple similar-sized videos
// at the folder root with no part markers.
type Classification struct {
	Parts     []ClassifiedFile `json:"parts"`
	Extras    []ClassifiedFile `json:"extras"`
	Ambiguous bool             `json:"ambiguous"`
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

	parts := detectParts(media)
	if len(parts) > 0 {
		cls.Parts = parts
		// Everything not in parts becomes an extra.
		partPaths := make(map[string]bool, len(parts))
		for _, p := range parts {
			partPaths[p.Path] = true
		}
		for _, m := range media {
			if partPaths[m.path] {
				continue
			}
			cls.Extras = append(cls.Extras, classifyExtra(folder, m))
		}
		return cls
	}

	// No part markers — split media into root-level videos and the rest.
	rootVideos, others := splitRootVideos(media)

	switch len(rootVideos) {
	case 0:
		// No top-level videos. Fall back to the legacy mainFile
		// heuristic: largest media file wins, with a root-over-nested
		// tie-breaker inside the 10% close band so a tiny teaser.mp3
		// at the root still beats a slightly-bigger track in audio/.
		main, rest := pickLargestWithRootTiebreak(media)
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
	return cls
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
// hit. The kind is derived from either the deepest subfolder name
// (case 4) or — for top-level files — a filename keyword scan
// (case 5). Image files under photos/pictures land on extra-photo;
// other top-level non-video files default to extra-other.
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
	out.SuggestedKind = ingest.AssignmentKindExtra(kindFromFilename(filepath.Base(m.path)))
	return out
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

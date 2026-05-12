package storage

import (
	"context"
	"fmt"
	"slices"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/collectionentry"
	"github.com/nicolerenee/promptbook/internal/ent/recordingversion"
	"github.com/nicolerenee/promptbook/internal/ent/wantsentry"
)

// Status is the derived, mutually-exclusive label for a recording's
// reconciled state across the local file inventory, the user's Encora
// collection, and the user's wants list.
type Status string

// The five possible reconciled states. See ComputeStatus for the precise
// rules that map (file present, in collection, in wants, format match) to
// one of these.
const (
	// StatusSynced means we have at least one file and either (a) the
	// recording is in the user's Encora collection and our locally-
	// computed format string matches collection.format, or (b) the
	// recording is on the wants list but not the collection — the user
	// owns what they wanted, even if Encora hasn't been told yet.
	StatusSynced Status = "synced"
	// StatusFormatMismatch means we have a file and the recording is in
	// the collection, but our computed format string differs from
	// collection.format. The user needs to either update Encora or
	// rescan the local file.
	StatusFormatMismatch Status = "format_mismatch"
	// StatusMissing means the recording is in the user's collection but
	// no file backs it locally.
	StatusMissing Status = "missing"
	// StatusWanted means the recording is on the wants list and no file
	// backs it locally.
	StatusWanted Status = "wanted"
	// StatusOrphan means there is a local file for a recording that is
	// neither in the collection nor on the wants list — typically a
	// stray import the user hasn't reconciled yet.
	StatusOrphan Status = "orphan"
)

// RecordingState bundles the booleans that drive ComputeStatus alongside
// the resolved Status. EncoraFormat is the collection.format value (empty
// when the recording isn't in the collection); LocalFormat is the
// ComputeFormatString output over the recording's versions (empty when no
// files are present).
type RecordingState struct {
	RecordingID  int64
	Status       Status
	InCollection bool
	InWants      bool
	FileCount    int
	EncoraFormat string // from collection.format; "" when not in collection.
	LocalFormat  string // ComputeFormatString(versions); "" when no files.
}

// ComputeStatus is a pure function from the boolean/format inputs of a
// RecordingState to its Status. Order of evaluation matters: a recording
// can sit in both the collection and the wants list at once (the user may
// want a higher-quality version of something they already own), and we
// prefer the collection-derived states when both apply.
//
// The order is:
//  1. file present + in collection + formats match → Synced
//  2. file present + in collection + formats differ → FormatMismatch
//  3. file present + not in collection + not in wants → Orphan
//  4. in wants + not in collection → Wanted (file presence is
//     orthogonal: the wants list itself can't carry a release format,
//     so a file-on-disk-but-only-in-wants recording is NOT "Synced"
//     in any meaningful sense. The user needs to promote it to the
//     collection before Synced applies.)
//  5. no file + in collection → Missing
//  6. fallback (no file, neither in collection nor wants) → Orphan
func ComputeStatus(s RecordingState) Status {
	hasFile := s.FileCount > 0
	switch {
	case hasFile && s.InCollection && s.EncoraFormat == s.LocalFormat:
		return StatusSynced
	case hasFile && s.InCollection:
		return StatusFormatMismatch
	case s.InWants && !s.InCollection:
		return StatusWanted
	case hasFile && !s.InCollection:
		return StatusOrphan
	case !hasFile && s.InCollection:
		return StatusMissing
	default:
		// Defensive fallback: no file, no collection, no wants. Shouldn't
		// happen in practice — there'd be no row to inspect — but treat
		// it as Orphan so callers always get a defined status.
		return StatusOrphan
	}
}

// LoadState resolves the RecordingState for a single recording. It pulls
// collection and wants membership directly, lists the recording's
// versions to derive both FileCount and LocalFormat, and finally calls
// ComputeStatus.
func LoadState(
	ctx context.Context, client *ent.Client, recordingID int64,
) (*RecordingState, error) {
	state := &RecordingState{RecordingID: recordingID}

	col, cerr := client.CollectionEntry.Query().
		Where(collectionentry.IDEQ(recordingID)).
		Only(ctx)
	if cerr != nil && !ent.IsNotFound(cerr) {
		return nil, fmt.Errorf("query collection for state: %w", cerr)
	}
	if col != nil {
		state.InCollection = true
		state.EncoraFormat = col.Format
	}

	exists, werr := client.WantsEntry.Query().
		Where(wantsentry.IDEQ(recordingID)).
		Exist(ctx)
	if werr != nil {
		return nil, fmt.Errorf("query wants for state: %w", werr)
	}
	state.InWants = exists

	versions, err := ListVersions(ctx, client, recordingID)
	if err != nil {
		return nil, fmt.Errorf("list versions for state: %w", err)
	}
	state.FileCount = len(versions)
	state.LocalFormat = ComputeFormatString(versions)
	state.Status = ComputeStatus(*state)
	return state, nil
}

// ListStatesOptions filters and paginates ListStates. Status is an
// any-of filter (empty slice means no status filter). Limit defaults to
// defaultListStatesLimit when zero; Offset defaults to zero.
type ListStatesOptions struct {
	Status []Status
	Limit  int
	Offset int
}

// defaultListStatesLimit is applied when ListStatesOptions.Limit is zero.
const defaultListStatesLimit = 200

// ListStates returns the reconciled state of every recording that
// appears in any of recordings, collection, wants, or recording_versions,
// optionally filtered to a subset of statuses.
//
// Implementation note: the union-of-three-tables approach uses three
// queries (collection ids, wants ids, version ids) merged in memory.
// Per-recording LocalFormat then requires another ListVersions call —
// N+1 on the result set, but each row is small. ent doesn't model SQL
// UNIONs directly without dropping into the raw modifier API, and the
// post-fetch ComputeStatus filtering already requires materializing
// every row; the three-query shape keeps the helper readable.
func ListStates(
	ctx context.Context,
	client *ent.Client,
	opts ListStatesOptions,
) ([]RecordingState, error) {
	limit := opts.Limit
	if limit == 0 {
		limit = defaultListStatesLimit
	}

	prelim, err := loadStatePrelim(ctx, client)
	if err != nil {
		return nil, err
	}

	wantStatuses := statusSet(opts.Status)
	out, err := finalizeStates(ctx, client, prelim, wantStatuses)
	if err != nil {
		return nil, err
	}

	if opts.Offset >= len(out) {
		return []RecordingState{}, nil
	}
	out = out[opts.Offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// loadStatePrelim materializes the per-recording (in_collection,
// in_wants, file_count, encora_format) slice that ListStates derives
// the final Status from. Pulled out of ListStates to keep the parent
// function under the gocognit threshold.
func loadStatePrelim(
	ctx context.Context, client *ent.Client,
) ([]RecordingState, error) {
	idSet, err := unionRecordingIDs(ctx, client)
	if err != nil {
		return nil, err
	}

	collectionRows, err := client.CollectionEntry.Query().All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query collection: %w", err)
	}
	collectionByID := make(map[int64]*ent.CollectionEntry, len(collectionRows))
	for _, c := range collectionRows {
		collectionByID[c.ID] = c
	}

	wantsRows, err := client.WantsEntry.Query().All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query wants: %w", err)
	}
	wantsByID := make(map[int64]struct{}, len(wantsRows))
	for _, w := range wantsRows {
		wantsByID[w.ID] = struct{}{}
	}

	var versionIDs []int64
	if err = client.RecordingVersion.Query().
		Select(recordingversion.FieldRecordingID).
		Scan(ctx, &versionIDs); err != nil {
		return nil, fmt.Errorf("query recording_versions: %w", err)
	}
	versionCount := make(map[int64]int, len(versionIDs))
	for _, id := range versionIDs {
		versionCount[id]++
	}

	ids := make([]int64, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	prelim := make([]RecordingState, 0, len(ids))
	for _, id := range ids {
		st := RecordingState{RecordingID: id, FileCount: versionCount[id]}
		if c, ok := collectionByID[id]; ok {
			st.InCollection = true
			st.EncoraFormat = c.Format
		}
		if _, ok := wantsByID[id]; ok {
			st.InWants = true
		}
		prelim = append(prelim, st)
	}
	return prelim, nil
}

// finalizeStates fills in LocalFormat (from ListVersions) and Status
// (via ComputeStatus) for each prelim row, then drops rows whose
// status isn't in wantStatuses (nil meaning "no filter").
func finalizeStates(
	ctx context.Context,
	client *ent.Client,
	prelim []RecordingState,
	wantStatuses map[Status]struct{},
) ([]RecordingState, error) {
	out := make([]RecordingState, 0, len(prelim))
	for _, st := range prelim {
		if st.FileCount > 0 {
			versions, verr := ListVersions(ctx, client, st.RecordingID)
			if verr != nil {
				return nil, fmt.Errorf(
					"list versions for recording %d: %w",
					st.RecordingID, verr,
				)
			}
			st.LocalFormat = ComputeFormatString(versions)
		}
		st.Status = ComputeStatus(st)
		if wantStatuses != nil {
			if _, ok := wantStatuses[st.Status]; !ok {
				continue
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// unionRecordingIDs returns the union of recording ids that appear in
// collection, wants, or recording_versions. Implemented as three plain
// id-only queries merged in a Go map — ent's predicate API can't
// express a SQL UNION without dropping into raw modifiers, and the
// alternative (hold every row in memory across three tables) is what
// the rest of ListStates does anyway.
func unionRecordingIDs(ctx context.Context, client *ent.Client) (map[int64]struct{}, error) {
	out := map[int64]struct{}{}

	colIDs, err := client.CollectionEntry.Query().IDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("query collection ids: %w", err)
	}
	for _, id := range colIDs {
		out[id] = struct{}{}
	}
	wantsIDs, err := client.WantsEntry.Query().IDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("query wants ids: %w", err)
	}
	for _, id := range wantsIDs {
		out[id] = struct{}{}
	}
	var versionRecIDs []int64
	if err = client.RecordingVersion.Query().
		Select(recordingversion.FieldRecordingID).
		Scan(ctx, &versionRecIDs); err != nil {
		return nil, fmt.Errorf("query recording_version recording ids: %w", err)
	}
	for _, id := range versionRecIDs {
		out[id] = struct{}{}
	}
	return out, nil
}

// statusSet returns nil for an empty slice (meaning "no filter") and a
// set-shaped map otherwise.
func statusSet(statuses []Status) map[Status]struct{} {
	if len(statuses) == 0 {
		return nil
	}
	m := make(map[Status]struct{}, len(statuses))
	for _, s := range statuses {
		m[s] = struct{}{}
	}
	return m
}

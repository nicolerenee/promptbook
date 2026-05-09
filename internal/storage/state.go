package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
//  4. file present + not in collection + in wants → Synced
//  5. no file + in collection → Missing
//  6. no file + in wants → Wanted
//  7. fallback (no file, neither in collection nor wants) → Orphan
func ComputeStatus(s RecordingState) Status {
	hasFile := s.FileCount > 0
	switch {
	case hasFile && s.InCollection && s.EncoraFormat == s.LocalFormat:
		return StatusSynced
	case hasFile && s.InCollection:
		return StatusFormatMismatch
	case hasFile && !s.InCollection && !s.InWants:
		return StatusOrphan
	case hasFile && !s.InCollection && s.InWants:
		return StatusSynced
	case !hasFile && s.InCollection:
		return StatusMissing
	case !hasFile && s.InWants:
		return StatusWanted
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
func LoadState(ctx context.Context, db *sql.DB, recordingID int64) (*RecordingState, error) {
	state := &RecordingState{RecordingID: recordingID}

	if err := db.QueryRowContext(ctx, `
		SELECT format FROM collection WHERE recording_id = ?
	`, recordingID).Scan(&state.EncoraFormat); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("query collection for state: %w", err)
		}
	} else {
		state.InCollection = true
	}

	var wantsMarker int
	if err := db.QueryRowContext(ctx, `
		SELECT 1 FROM wants WHERE recording_id = ?
	`, recordingID).Scan(&wantsMarker); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("query wants for state: %w", err)
		}
	} else {
		state.InWants = true
	}

	versions, err := ListVersions(ctx, db, recordingID)
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
// Implementation note: the LEFT-JOIN-and-aggregate approach gathers
// collection / wants / version-count in a single SQL pass. Computing
// LocalFormat correctly requires the per-version FormatLabel ordered by
// (file_size_bytes DESC, id ASC) — SQLite group_concat doesn't honor
// per-row ordering reliably across versions, so we do that step
// separately by calling ListVersions per recording. This is N+1 on the
// number of recordings, but each row read is small and the alternative
// (a window function + post-aggregation reassembly) wouldn't materially
// reduce IO. If this ever shows up in a profile, switch to a single
// query using SQLite's row_number() ordering.
func ListStates(
	ctx context.Context,
	db *sql.DB,
	opts ListStatesOptions,
) ([]RecordingState, error) {
	limit := opts.Limit
	if limit == 0 {
		limit = defaultListStatesLimit
	}

	// Union the recording_id sets from every table that can contribute a
	// state row, then LEFT JOIN back to collection / wants / a
	// version-count subquery. Pagination is applied post-aggregation.
	const q = `
		WITH ids AS (
			SELECT recording_id FROM collection
			UNION
			SELECT recording_id FROM wants
			UNION
			SELECT recording_id FROM recording_versions
		),
		version_counts AS (
			SELECT recording_id, COUNT(*) AS file_count
			FROM recording_versions
			GROUP BY recording_id
		)
		SELECT
			i.recording_id,
			COALESCE(c.format, '')                AS encora_format,
			CASE WHEN c.recording_id IS NULL THEN 0 ELSE 1 END AS in_collection,
			CASE WHEN w.recording_id IS NULL THEN 0 ELSE 1 END AS in_wants,
			COALESCE(v.file_count, 0)             AS file_count
		FROM ids AS i
		LEFT JOIN collection      AS c ON c.recording_id = i.recording_id
		LEFT JOIN wants           AS w ON w.recording_id = i.recording_id
		LEFT JOIN version_counts  AS v ON v.recording_id = i.recording_id
		ORDER BY i.recording_id ASC
	`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query list states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	wantStatuses := statusSet(opts.Status)

	// Materialize first so we can apply status filtering and pagination
	// after the per-recording LocalFormat fill. We can't push the filter
	// down to SQL because Status is derived from data (the format
	// equality check) we don't fully have until LocalFormat is known.
	var prelim []RecordingState
	for rows.Next() {
		var (
			st           RecordingState
			inCollection int
			inWants      int
		)
		if scanErr := rows.Scan(
			&st.RecordingID,
			&st.EncoraFormat,
			&inCollection,
			&inWants,
			&st.FileCount,
		); scanErr != nil {
			return nil, fmt.Errorf("scan state row: %w", scanErr)
		}
		st.InCollection = inCollection != 0
		st.InWants = inWants != 0
		prelim = append(prelim, st)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate state rows: %w", rerr)
	}

	out := make([]RecordingState, 0, len(prelim))
	for _, st := range prelim {
		if st.FileCount > 0 {
			versions, verr := ListVersions(ctx, db, st.RecordingID)
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

	// Apply pagination after status filtering so callers asking for
	// "page 2 of FormatMismatch" see the right slice.
	if opts.Offset >= len(out) {
		return []RecordingState{}, nil
	}
	out = out[opts.Offset:]
	if len(out) > limit {
		out = out[:limit]
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

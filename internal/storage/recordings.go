package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/collectionentry"
	"github.com/nicolerenee/promptbook/internal/ent/recording"
	"github.com/nicolerenee/promptbook/internal/ent/wantsentry"
)

// ErrRecordingNotFound signals that no recording with the given id is in
// the local cache. Distinct from the encora-package error so callers can
// disambiguate "missing locally" from "missing upstream".
var ErrRecordingNotFound = errors.New("storage: recording not found")

// LoadedRecording bundles the recording payload with the local-only fields
// that are useful when printing detail (collection state, sync time). The
// Versions, LocalFormatString, and Cast fields are populated from the
// people + recording_versions tables so callers (UI page, CLI show) can
// render a full detail view in one round trip; they are additive to the
// existing fields and don't replace anything in encora.Recording.
type LoadedRecording struct {
	Recording         encora.Recording
	InCollection      bool
	InWants           bool
	Format            string
	UserNotes         *string
	UserWatched       bool
	CollectedAt       *string
	LastSyncedAt      *string
	RawJSONPresent    bool
	Versions          []RecordingVersion
	LocalFormatString string
	Cast              []ResolvedCastEntry
	// ExternallyManaged mirrors recordings.externally_managed — true
	// when an external tool (Radarr/Plex/Jellyfin) owns the files on
	// disk and promptbook only catalogs the recording. The recording
	// detail page hides rename / NFO / move controls in this mode;
	// the toggle flips it via setRecordingExternallyManaged.
	ExternallyManaged bool
	// PrivateNotes is user-owned free text that never syncs to Encora.
	// Empty string is the "no notes" sentinel. Edited from the
	// recording detail page via setRecordingPrivateNotes.
	PrivateNotes string
}

// ResolvedCastEntry pairs a cast row with the canonical performer +
// character entities from the people tables. Useful when callers want
// a single click-through path; the embedded encora.Recording.Cast
// remains the source of truth for cast ORDER and Status.
type ResolvedCastEntry struct {
	Performer Performer // from storage.LoadPerformer; zero-valued if missing.
	Character Character // from storage.LoadCharacter; zero-valued if missing.
	Status    *encora.CastStatus
	Order     int
}

// LoadRecording fetches one recording from the local cache by Encora ID,
// reconstituting it from the raw_json column rather than re-assembling
// from denormalized fields.
func LoadRecording(
	ctx context.Context, client *ent.Client, id int64,
) (*LoadedRecording, error) {
	row, err := client.Recording.Get(ctx, id)
	if ent.IsNotFound(err) {
		return nil, fmt.Errorf("recording %d: %w", id, ErrRecordingNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("query recording: %w", err)
	}

	r, err := decodeRecording(row.RawJSON)
	if err != nil {
		return nil, fmt.Errorf("decode recording %d: %w", id, err)
	}
	loaded := &LoadedRecording{
		Recording:         r,
		RawJSONPresent:    true,
		ExternallyManaged: row.ExternallyManaged,
		PrivateNotes:      row.PrivateNotes,
	}

	if cerr := fillCollectionState(ctx, client, id, loaded); cerr != nil {
		return nil, cerr
	}
	if werr := fillWantsState(ctx, client, id, loaded); werr != nil {
		return nil, werr
	}
	if verr := fillVersions(ctx, client, id, loaded); verr != nil {
		return nil, verr
	}
	if cerr := fillResolvedCast(ctx, client, loaded); cerr != nil {
		return nil, cerr
	}
	return loaded, nil
}

// fillVersions loads every recording_versions row for the recording and
// computes the canonical local format string. The Versions slice is
// always non-nil; callers can range over it without a length check.
func fillVersions(
	ctx context.Context,
	client *ent.Client,
	id int64,
	loaded *LoadedRecording,
) error {
	versions, err := ListVersions(ctx, client, id)
	if err != nil {
		return fmt.Errorf("list versions for recording %d: %w", id, err)
	}
	if versions == nil {
		versions = []RecordingVersion{}
	}
	loaded.Versions = versions
	loaded.LocalFormatString = ComputeFormatString(versions)
	return nil
}

// fillResolvedCast walks the denormalized encora.Recording.Cast and pairs
// each entry with the canonical performer/character rows from the people
// tables. Missing people rows (legacy data not yet promoted) are tolerated
// silently — the corresponding ResolvedCastEntry keeps a zero-valued
// Performer or Character and we move on.
func fillResolvedCast(
	ctx context.Context,
	client *ent.Client,
	loaded *LoadedRecording,
) error {
	cast := loaded.Recording.Cast
	resolved := make([]ResolvedCastEntry, 0, len(cast))
	for _, entry := range cast {
		re := ResolvedCastEntry{
			Status: entry.Status,
			Order:  entry.Character.Order,
		}
		if p, perr := LoadPerformer(ctx, client, entry.Performer.ID); perr == nil {
			re.Performer = *p
		} else if !errors.Is(perr, ErrPerformerNotFound) {
			return fmt.Errorf("load performer %d: %w", entry.Performer.ID, perr)
		}
		if c, cerr := LoadCharacter(ctx, client, entry.Character.ID); cerr == nil {
			re.Character = *c
		} else if !errors.Is(cerr, ErrCharacterNotFound) {
			return fmt.Errorf("load character %d: %w", entry.Character.ID, cerr)
		}
		resolved = append(resolved, re)
	}
	loaded.Cast = resolved
	return nil
}

// SetCollectionFormat updates collection_entries.format for the given
// recording. Used after a successful "Update format on Encora" push so
// the local mismatch resolves immediately without waiting for the
// next full sync. Returns ErrCollectionEntryNotFound when the
// recording isn't in the user's collection — defensive; the apply
// handler should only invoke this on a confirmed collection-mismatch
// row.
func SetCollectionFormat(
	ctx context.Context, client *ent.Client, recordingID int64, format string,
) error {
	n, err := client.CollectionEntry.Update().
		Where(collectionentry.IDEQ(recordingID)).
		SetFormat(format).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("update collection_entries.format for %d: %w", recordingID, err)
	}
	if n == 0 {
		return ErrCollectionEntryNotFound
	}
	return nil
}

// ErrCollectionEntryNotFound is returned by SetCollectionFormat when
// the recording id has no row in collection_entries.
var ErrCollectionEntryNotFound = errors.New("storage: collection entry not found")

// SetRecordingExternallyManaged flips the recordings.externally_managed
// flag for the given recording. Used by the ingest pipeline (when an
// import opted into externally-managed mode) and by the recording
// detail page's setRecordingExternallyManaged toggle. Returns
// ErrRecordingNotFound when the row is missing — defensive; callers
// shouldn't reach this path without a confirmed recording id.
func SetRecordingExternallyManaged(
	ctx context.Context, client *ent.Client, recordingID int64, externallyManaged bool,
) error {
	n, err := client.Recording.Update().
		Where(recording.IDEQ(recordingID)).
		SetExternallyManaged(externallyManaged).
		Save(ctx)
	if err != nil {
		return fmt.Errorf(
			"update recordings.externally_managed for %d: %w", recordingID, err)
	}
	if n == 0 {
		return fmt.Errorf("recording %d: %w", recordingID, ErrRecordingNotFound)
	}
	return nil
}

// SetRecordingPrivateNotes overwrites recordings.private_notes for the
// given recording. Empty string is valid (the "no notes" sentinel).
// Returns ErrRecordingNotFound when the row is missing.
func SetRecordingPrivateNotes(
	ctx context.Context, client *ent.Client, recordingID int64, notes string,
) error {
	n, err := client.Recording.Update().
		Where(recording.IDEQ(recordingID)).
		SetPrivateNotes(notes).
		Save(ctx)
	if err != nil {
		return fmt.Errorf(
			"update recordings.private_notes for %d: %w", recordingID, err)
	}
	if n == 0 {
		return fmt.Errorf("recording %d: %w", recordingID, ErrRecordingNotFound)
	}
	return nil
}

func fillCollectionState(
	ctx context.Context,
	client *ent.Client,
	id int64,
	loaded *LoadedRecording,
) error {
	row, err := client.CollectionEntry.Query().
		Where(collectionentry.IDEQ(id)).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("query collection: %w", err)
	}
	loaded.InCollection = true
	loaded.Format = row.Format
	loaded.UserWatched = row.UserWatched
	if row.UserNotes != nil {
		s := *row.UserNotes
		loaded.UserNotes = &s
	}
	if row.CollectedAt != nil {
		s := row.CollectedAt.Format("2006-01-02 15:04:05")
		loaded.CollectedAt = &s
	}
	s := row.LastSyncedAt.Format("2006-01-02 15:04:05")
	loaded.LastSyncedAt = &s
	return nil
}

func fillWantsState(
	ctx context.Context,
	client *ent.Client,
	id int64,
	loaded *LoadedRecording,
) error {
	row, err := client.WantsEntry.Query().
		Where(wantsentry.IDEQ(id)).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("query wants: %w", err)
	}
	loaded.InWants = true
	if loaded.LastSyncedAt == nil {
		s := row.LastSyncedAt.Format("2006-01-02 15:04:05")
		loaded.LastSyncedAt = &s
	}
	return nil
}

// LoadRecordingRaw fetches the raw_json blob for a recording and returns
// it as a parsed encora.Recording without doing the collection / wants /
// versions / cast joins. Callers that only need the upstream payload
// avoid four extra round trips this way. ErrRecordingNotFound is
// returned when no row is present.
func LoadRecordingRaw(
	ctx context.Context, client *ent.Client, id int64,
) (encora.Recording, error) {
	row, err := client.Recording.Query().
		Where(recording.IDEQ(id)).
		Only(ctx)
	if ent.IsNotFound(err) {
		return encora.Recording{}, fmt.Errorf("recording %d: %w", id, ErrRecordingNotFound)
	}
	if err != nil {
		return encora.Recording{}, fmt.Errorf("query recording: %w", err)
	}
	r, err := decodeRecording(row.RawJSON)
	if err != nil {
		return encora.Recording{}, fmt.Errorf("decode recording %d: %w", id, err)
	}
	return r, nil
}

// ParseRecordingID validates a CLI-supplied id string.
func ParseRecordingID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse id %q: %w", s, err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("invalid id %d", id)
	}
	return id, nil
}

func decodeRecording(rawJSON string) (encora.Recording, error) {
	var r encora.Recording
	if err := json.Unmarshal([]byte(rawJSON), &r); err != nil {
		return r, err
	}
	return r, nil
}

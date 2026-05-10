package graph

// prefixed_id.go — context-aware marshalers for the GraphQL ID scalar.
//
// The wire format is `<type>-<int64>` (e.g. `recording-1234`,
// `show-5678`). Internally every entity still uses int64 PKs; the
// prefix is applied at the GraphQL boundary only.
//
// gqlgen picks up `MarshalPrefixedID` + `UnmarshalPrefixedID` because
// gqlgen.yml maps the `ID` scalar to
// `github.com/nicolerenee/promptbook/internal/server/graph.PrefixedID`.
// gqlgen looks up `Marshal<Name>` / `Unmarshal<Name>` package-level
// functions when the bound model is the function pair pattern (the
// same pattern used for graphql.Int64). Returning a
// graphql.ContextMarshaler trips gqlgen's IsContext detection so the
// generated marshaler wraps every call through WrapContextMarshaler,
// giving us the FieldContext we need to derive the prefix from the
// owning type + field name.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/99designs/gqlgen/graphql"
)

// PrefixedID is a marker type alias so gqlgen's binder finds something
// to bind to under that name. The underlying Go representation is
// int64 — every ent ID is int64 — and gqlgen reads the function
// signature of MarshalPrefixedID to discover that. The alias keeps the
// type name visible in generated code and stack traces.
type PrefixedID = int64

// nodePrefix is the per-entity prefix used on the wire. Lifted into a
// constant block so the table reads as a single source-of-truth — a
// rename here is the only place to keep the marshal + unmarshal sides
// consistent.
const (
	prefixCastEntry        = "cast"
	prefixCollectionEntry  = "collection"
	prefixPerformer        = "performer"
	prefixQueue            = "queue"
	prefixRecording        = "recording"
	prefixRecordingVersion = "version"
	prefixShow             = "show"
	prefixSyncRun          = "sync"
	prefixWantsEntry       = "wants"
	// prefixCharacter is the FK-only prefix for the resolved-cast
	// entry's characterID slot. We don't expose a top-level Character
	// node in GraphQL (characters live behind ResolvedCastEntry), so
	// this prefix is purely cosmetic — it keeps the wire shape
	// self-describing.
	prefixCharacter = "character"
)

// fkFieldShowID / fkFieldRecordingID etc. — pulled into constants so
// the entries in foreignKeyFieldPrefix below + helpers don't repeat
// the same string literal across files (goconst pings duplicates).
const (
	fkFieldShowID           = "showID"
	fkFieldShowIDNEQ        = "showIDNEQ"
	fkFieldShowIDIn         = "showIDIn"
	fkFieldShowIDNotIn      = "showIDNotIn"
	fkFieldRecordingID      = "recordingID"
	fkFieldRecordingIDNEQ   = "recordingIDNEQ"
	fkFieldRecordingIDIn    = "recordingIDIn"
	fkFieldRecordingIDNotIn = "recordingIDNotIn"
	fkFieldCastEntryID      = "castEntryID"
	fkFieldPerformerID      = "performerID"
	fkFieldCharacterID      = "characterID"
	// fkFieldSuggestedRecordingID is the QueueEntry field that points
	// at the scanner's best-guess recording. Null when the scanner had
	// no candidate; non-null values marshal as "recording-N".
	fkFieldSuggestedRecordingID = "suggestedRecordingID"
)

// objectName* — GraphQL Object names that show up in two or more
// places (objectIDPrefix + tests + helpers). Extracted into constants
// so goconst stops pinging the duplicates.
const (
	objectNameCastEntry             = "CastEntry"
	objectNameCastEntryWhereInput   = "CastEntryWhereInput"
	objectNameCollectionEntry       = "CollectionEntry"
	objectNameCollectionEntryWhere  = "CollectionEntryWhereInput"
	objectNamePerformer             = "Performer"
	objectNamePerformerWhereInput   = "PerformerWhereInput"
	objectNameRecording             = "Recording"
	objectNameRecordingWhereInput   = "RecordingWhereInput"
	objectNameRecordingVersion      = "RecordingVersion"
	objectNameRecordingVersionWhere = "RecordingVersionWhereInput"
	objectNameShow                  = "Show"
	objectNameShowWhereInput        = "ShowWhereInput"
	objectNameSyncRun               = "SyncRun"
	objectNameSyncRunWhereInput     = "SyncRunWhereInput"
	objectNameWantsEntry            = "WantsEntry"
	objectNameWantsEntryWhereInput  = "WantsEntryWhereInput"
	objectNameRecordingsListItem    = "RecordingsListItem"
	objectNameShowsListItem         = "ShowsListItem"
	objectNamePersonListItem        = "PersonListItem"
	objectNamePersonRecording       = "PersonRecording"
	objectNamePersonDetail          = "PersonDetail"
	objectNameQueueEntry            = "QueueEntry"
	objectNameImportQueueEntryInput = "ImportQueueEntryInput"
)

// objectIDPrefix maps a GraphQL Object name (the type the field
// belongs to) to the prefix used for that type's `id` field. Drives
// the marshal path when the FieldContext exposes Object="Recording"
// for a Recording.id selection. Input types like RecordingWhereInput
// are mapped here too because their id-shaped predicates filter rows
// of the same entity.
//
//nolint:gochecknoglobals // Lookup table; unchanging program data.
var objectIDPrefix = map[string]string{
	objectNameCastEntry:             prefixCastEntry,
	objectNameCastEntryWhereInput:   prefixCastEntry,
	objectNameCollectionEntry:       prefixCollectionEntry,
	objectNameCollectionEntryWhere:  prefixCollectionEntry,
	objectNamePerformer:             prefixPerformer,
	objectNamePerformerWhereInput:   prefixPerformer,
	objectNameRecording:             prefixRecording,
	objectNameRecordingWhereInput:   prefixRecording,
	objectNameRecordingVersion:      prefixRecordingVersion,
	objectNameRecordingVersionWhere: prefixRecordingVersion,
	objectNameShow:                  prefixShow,
	objectNameShowWhereInput:        prefixShow,
	objectNameSyncRun:               prefixSyncRun,
	objectNameSyncRunWhereInput:     prefixSyncRun,
	objectNameWantsEntry:            prefixWantsEntry,
	objectNameWantsEntryWhereInput:  prefixWantsEntry,
	// Custom enrichment types — RecordingsListItem represents a row
	// from the recordingsList query envelope; ShowsListItem mirrors
	// the by-show aggregate. ResolvedCastEntry's `castEntryID` field
	// is captured by foreignKeyFieldPrefix below; its top-level `id`
	// field doesn't exist on the type so the Object-level entry isn't
	// strictly necessary.
	objectNameRecordingsListItem: prefixRecording,
	objectNameShowsListItem:      prefixShow,
	// People surface — PersonListItem.performerID + PersonDetail.performerID
	// are captured by foreignKeyFieldPrefix below; PersonRecording.id
	// is a recording reference, surfaced via the type-default mapping
	// here.
	objectNamePersonRecording: prefixRecording,
	// Queue surface — QueueEntry.id maps to "queue-N";
	// ImportQueueEntryInput.queueID is the same prefix (the input's id
	// is also a queue reference). suggestedRecordingID + recordingID
	// fields fall through to foreignKeyFieldPrefix.
	objectNameQueueEntry:            prefixQueue,
	objectNameImportQueueEntryInput: prefixQueue,
}

// foreignKeyFieldPrefix maps an ID-typed field name (the foreign-key
// shape on a node or where-input) to the prefix of the entity it
// references. e.g. `Recording.showID` and `ShowWhereInput.id*` both
// resolve through `objectIDPrefix`, but `Recording.showID` (where the
// field name itself encodes the FK target) is what this table covers.
//
// Order: the table is consulted AFTER the field's owning Object falls
// out as the default. So `id` / `idIn` / `idGT` etc on
// RecordingWhereInput are still recording-prefixed via objectIDPrefix;
// this table only fires for FK fields like `showID`, `recordingID`.
//
//nolint:gochecknoglobals // Lookup table; unchanging program data.
var foreignKeyFieldPrefix = map[string]string{
	fkFieldRecordingID:      prefixRecording,
	fkFieldRecordingIDNEQ:   prefixRecording,
	fkFieldRecordingIDIn:    prefixRecording,
	fkFieldRecordingIDNotIn: prefixRecording,
	fkFieldShowID:           prefixShow,
	fkFieldShowIDNEQ:        prefixShow,
	fkFieldShowIDIn:         prefixShow,
	fkFieldShowIDNotIn:      prefixShow,
	// ResolvedCastEntry custom-type FK fields: castEntryID points at
	// the cast row, performerID at the canonical performer.
	// characterID is `ID` (nullable) but maps to character entries
	// (which we don't expose as a top-level entity in GraphQL — the
	// prefix is purely cosmetic but kept for shape consistency).
	fkFieldCastEntryID: prefixCastEntry,
	fkFieldPerformerID: prefixPerformer,
	fkFieldCharacterID: prefixCharacter,
	// QueueEntry.suggestedRecordingID points at a recording row; the
	// import-mutation input's recordingID slot is already covered by
	// fkFieldRecordingID above.
	fkFieldSuggestedRecordingID: prefixRecording,
}

// validPrefixes is the set of prefixes Unmarshal accepts when the
// FieldContext doesn't pin a single expected prefix (e.g. the
// node(id:) resolver argument). Mirrors objectIDPrefix's value set.
//
//nolint:gochecknoglobals // Lookup table; unchanging program data.
var validPrefixes = map[string]bool{
	prefixCastEntry:        true,
	prefixCollectionEntry:  true,
	prefixPerformer:        true,
	prefixQueue:            true,
	prefixRecording:        true,
	prefixRecordingVersion: true,
	prefixShow:             true,
	prefixSyncRun:          true,
	prefixWantsEntry:       true,
	prefixCharacter:        true,
}

// errPrefixedIDFormat is the wire-level "this string isn't a valid
// prefixed id" error. Wrapped at every call site so the gqlgen error
// path includes the field path that produced the bad value.
var errPrefixedIDFormat = errors.New("prefixed id: must be of the form <type>-<int64>")

// MarshalPrefixedID writes id as `<prefix>-<int64>` where prefix is
// derived from the FieldContext. Returning a graphql.ContextMarshaler
// signals to gqlgen's codegen that the ctx must be plumbed through
// the marshaler call (see codegen/binder.go IsContext detection).
func MarshalPrefixedID(id int64) graphql.ContextMarshaler {
	return graphql.ContextWriterFunc(func(ctx context.Context, w io.Writer) error {
		prefix := prefixForFieldContext(graphql.GetFieldContext(ctx))
		if prefix == "" {
			// Defensive: an ID field with no resolvable prefix would
			// produce a wire value the unmarshal side can't accept.
			// Surface the shape so test failures point to the missing
			// mapping table entry rather than a confusing 4xx.
			return errors.New("graph: marshal prefixed id: no prefix " +
				"registered for object/field — extend objectIDPrefix " +
				"or foreignKeyFieldPrefix in prefixed_id.go")
		}
		_, err := io.WriteString(w, strconv.Quote(prefix+"-"+strconv.FormatInt(id, 10)))
		return err
	})
}

// UnmarshalPrefixedID parses a wire-format `<prefix>-<int64>` string
// back into the int64 ent uses. Validates that the prefix matches the
// receiving field's expected type so a `show-1` value passed where
// `recording-...` is required surfaces a clear error rather than
// silently loading the wrong row. The node(id:) resolver argument is
// the one place we accept any prefix — see `Field.Name == "node"`
// branch.
func UnmarshalPrefixedID(ctx context.Context, v any) (int64, error) {
	s, err := coerceIDString(v)
	if err != nil {
		return 0, err
	}
	prefix, idStr, ok := strings.Cut(s, "-")
	if !ok || prefix == "" || idStr == "" {
		return 0, fmt.Errorf("%w: got %q", errPrefixedIDFormat, s)
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: parse int %q: %s", errPrefixedIDFormat, idStr, err.Error())
	}
	if !validPrefixes[prefix] {
		return 0, fmt.Errorf("%w: unknown prefix %q", errPrefixedIDFormat, prefix)
	}
	expected := expectedPrefixForFieldContext(graphql.GetFieldContext(ctx))
	if expected != "" && prefix != expected {
		return 0, fmt.Errorf("%w: expected %q prefix, got %q", errPrefixedIDFormat, expected, prefix)
	}
	return id, nil
}

// coerceIDString accepts the json shapes gqlgen passes for an ID
// argument (string, json.Number-as-string fallback). int64-encoded
// numbers are rejected — the wire format is the prefixed string. This
// is the place that breaks legacy callers passing a bare integer ID.
func coerceIDString(v any) (string, error) {
	if v == nil {
		return "", fmt.Errorf("%w: got null", errPrefixedIDFormat)
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", fmt.Errorf("%w: expected string, got %T", errPrefixedIDFormat, v)
}

// prefixForFieldContext walks the FieldContext to pick the right
// prefix for the field being marshaled. Field name takes precedence
// for FK fields (showID, recordingID); the owning Object covers the
// `id` field on every node + where-input.
func prefixForFieldContext(fc *graphql.FieldContext) string {
	if fc == nil {
		return ""
	}
	if p, ok := foreignKeyFieldPrefix[fc.Field.Name]; ok {
		return p
	}
	if p, ok := objectIDPrefix[fc.Object]; ok {
		return p
	}
	return ""
}

// queryRoot / nodeField / nodesField are the GraphQL spellings of
// the Relay node-dispatch surface — pulled out as constants so the
// prefix-validation skip in expectedPrefixForFieldContext stays
// readable + grep-friendly across the package.
const (
	queryRoot  = "Query"
	nodeField  = "node"
	nodesField = "nodes"
)

// queryFieldPrefix maps a Query.<field> name (the typed by-id
// shortcuts in schema.graphql) to the prefix the field's `id`
// argument should carry. e.g. recording(id:) requires the
// "recording" prefix on its argument; show(id:) requires "show".
// Keeps the by-id query shortcuts type-safe so a `recording-1234`
// passed to show(id:) trips the prefix check rather than silently
// loading the wrong row.
//
// The keys happen to match the prefix-constant values for the
// canonical entities — that's no accident, the schema names line up
// with the prefix tokens. Wired via the prefix constants so a future
// rename ripples through both surfaces.
//
//nolint:gochecknoglobals // Lookup table; unchanging program data.
var queryFieldPrefix = map[string]string{
	prefixRecording:       prefixRecording,
	prefixShow:            prefixShow,
	prefixPerformer:       prefixPerformer,
	prefixCollectionEntry: prefixCollectionEntry,
	prefixWantsEntry:      prefixWantsEntry,
	// person(id:) returns the PersonDetail enrichment shape — its
	// argument carries the same "performer-N" prefix as the canonical
	// performer(id:) shortcut.
	"person": prefixPerformer,
}

// expectedPrefixForFieldContext is the unmarshal-side analog of
// prefixForFieldContext. Returns "" when the field is the special
// node/nodes argument that accepts any prefix. The empty return value
// disables the prefix validation in UnmarshalPrefixedID.
func expectedPrefixForFieldContext(fc *graphql.FieldContext) string {
	if fc == nil {
		return ""
	}
	// node(id:) and nodes(ids:) on the Query root accept any prefix
	// because their job is precisely to dispatch on the prefix. The
	// resolver itself enforces validity.
	if fc.Object == queryRoot {
		switch fc.Field.Name {
		case nodeField, nodesField:
			return ""
		}
		// Typed by-id shortcuts (recording(id:), show(id:), …) carry
		// a per-field expected prefix. Without this branch a
		// `show-1234` argument to recording(id:) would silently
		// dispatch through the recording loader.
		if p, ok := queryFieldPrefix[fc.Field.Name]; ok {
			return p
		}
	}
	return prefixForFieldContext(fc)
}

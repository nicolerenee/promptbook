package graph

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/99designs/gqlgen/graphql"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/castentry"
	"github.com/nicolerenee/promptbook/internal/ent/collectionentry"
	"github.com/nicolerenee/promptbook/internal/ent/performer"
	"github.com/nicolerenee/promptbook/internal/ent/recording"
	"github.com/nicolerenee/promptbook/internal/ent/recordingversion"
	"github.com/nicolerenee/promptbook/internal/ent/show"
	"github.com/nicolerenee/promptbook/internal/ent/syncrun"
	"github.com/nicolerenee/promptbook/internal/ent/wantsentry"
)

// nilOnNotFound returns (nil, nil) when err is an ent NotFoundError so
// the gqlgen field resolver renders a `null` for the GraphQL field
// (the GraphQL convention for "no such row") instead of bubbling a
// hard error. Any other error passes through wrapped for context.
//
// (nil, nil) is the GraphQL idiom for "field exists, no value" — the
// alternative would be making the per-type fetchers non-null and
// returning errors for missing rows, which is strictly worse UX.
//
// Lives in this file (not the gqlgen-managed *.resolvers.go) so the
// helper survives gqlgen regeneration runs.
func nilOnNotFound[T any](node *T, err error, what string) (*T, error) {
	if err == nil {
		return node, nil
	}
	var nfe *ent.NotFoundError
	if errors.As(err, &nfe) {
		return nil, nil //nolint:nilnil // (nil, nil) signals "no row" to gqlgen.
	}
	return nil, fmt.Errorf("graphql: load %s: %w", what, err)
}

// prefixToTable maps the wire-format prefix of a PrefixedID back to
// the ent table name the noder dispatcher expects. Mirrors the prefix
// constants in prefixed_id.go; kept as a separate table so adding a
// new entity is a single-line change in two places (prefix const +
// this map) rather than a sprawl across files.
//
//nolint:gochecknoglobals // Lookup table; unchanging program data.
var prefixToTable = map[string]string{
	prefixCastEntry:        castentry.Table,
	prefixCollectionEntry:  collectionentry.Table,
	prefixPerformer:        performer.Table,
	prefixRecording:        recording.Table,
	prefixRecordingVersion: recordingversion.Table,
	prefixShow:             show.Table,
	prefixSyncRun:          syncrun.Table,
	prefixWantsEntry:       wantsentry.Table,
}

// nodeTableForArg returns the ent table name for the prefix carried
// by the node(id:) resolver's input argument. The PrefixedID
// unmarshaler has already validated the wire-format and stripped the
// prefix; we re-pull the original string off FieldContext.Args so the
// dispatch knows which entity to load.
func nodeTableForArg(ctx context.Context) (string, error) {
	fc := graphql.GetFieldContext(ctx)
	if fc == nil {
		return "", errors.New("graphql: node resolver: no field context")
	}
	rawAny, ok := fc.Args["id"]
	if !ok {
		return "", errors.New("graphql: node resolver: missing id arg")
	}
	// fc.Args["id"] holds the parsed int64 (post-unmarshal) — the raw
	// string is gone by the time the resolver fires. We need to walk
	// the operation context to recover the original variable / literal.
	// Easier path: grab the raw from the OperationContext's Variables
	// or the field's selection arguments.
	if id, isInt := rawAny.(int64); isInt {
		// The unmarshaler stashed an int64. Look at the field's
		// arguments AST to find the original raw string and re-derive
		// the prefix. The raw value lives in fc.Field.Arguments.
		raw := rawArgValue(ctx, fc, "id")
		if raw == "" {
			return "", fmt.Errorf("graphql: node(%d): could not recover prefix from raw arg", id)
		}
		return tableForRawID(raw)
	}
	return "", fmt.Errorf("graphql: node resolver: unexpected arg type %T", rawAny)
}

// nodeRawIDsForArg returns the raw `<prefix>-<int64>` strings for the
// nodes(ids:) resolver. Same prefix-recovery problem as
// nodeTableForArg, just with a slice.
func nodeRawIDsForArg(ctx context.Context) ([]string, error) {
	fc := graphql.GetFieldContext(ctx)
	if fc == nil {
		return nil, errors.New("graphql: nodes resolver: no field context")
	}
	raws := rawArgValues(ctx, fc, "ids")
	if raws == nil {
		return nil, errors.New("graphql: nodes resolver: could not recover ids arg")
	}
	return raws, nil
}

// tableForRawID parses a `<prefix>-<int64>` string and returns the
// ent table name. The prefix is validated against prefixToTable; an
// unknown prefix surfaces the same shape-error UnmarshalPrefixedID
// would have produced — defense in depth in case a caller bypasses
// the unmarshal path.
func tableForRawID(raw string) (string, error) {
	prefix, _, ok := strings.Cut(raw, "-")
	if !ok || prefix == "" {
		return "", fmt.Errorf("graphql: node id %q: missing prefix", raw)
	}
	table, ok := prefixToTable[prefix]
	if !ok {
		return "", fmt.Errorf("graphql: node id %q: unknown prefix %q", raw, prefix)
	}
	return table, nil
}

// rawArgValue returns the source-text representation of a scalar
// argument as written in the original GraphQL document (or the
// referenced variable). gqlgen exposes Args (parsed values) but not
// the raw — we walk fc.Field.Arguments to find it.
func rawArgValue(ctx context.Context, fc *graphql.FieldContext, name string) string {
	if fc == nil || fc.Field.Field == nil {
		return ""
	}
	for _, a := range fc.Field.Arguments {
		if a.Name != name {
			continue
		}
		if a.Value == nil {
			return ""
		}
		raw, err := a.Value.Value(graphql.GetOperationContext(ctx).Variables)
		if err != nil {
			return ""
		}
		if s, ok := raw.(string); ok {
			return s
		}
		return ""
	}
	return ""
}

// stringReplacer is the minimal HTML→text rewriter the show
// description resolver runs on raw_json.metadata.show_description.
// Mirrors the legacy REST stripShowDescriptionHTML.
//
//nolint:gochecknoglobals // strings.Replacer is immutable; one instance keeps allocations down.
var stringReplacer = strings.NewReplacer(
	"<p>", "",
	"</p>", "\n\n",
	"<br>", "\n",
	"<br/>", "\n",
	"<br />", "\n",
	"&#039;", "'",
	"&quot;", `"`,
	"&amp;", "&",
	"&lt;", "<",
	"&gt;", ">",
)

// yearPrefixLen is the number of leading characters of date_full
// parseYearPrefix inspects. Encora date_full strings start with YYYY.
const yearPrefixLen = 4

// minYear / maxYear are the inclusive sanity bounds parseYearPrefix
// accepts. Mirrors the legacy REST helper.
const (
	minYear = 1000
	maxYear = 9999
)

// parseYearPrefix extracts a 4-digit year from the leading characters
// of an Encora date_full string. Returns (0, false) when the string
// is too short or the leading 4 chars don't parse as an in-bounds
// integer.
func parseYearPrefix(dateFull string) (int, bool) {
	if len(dateFull) < yearPrefixLen {
		return 0, false
	}
	y, err := strconv.Atoi(dateFull[:yearPrefixLen])
	if err != nil || y < minYear || y > maxYear {
		return 0, false
	}
	return y, true
}

// rawArgValues is rawArgValue's slice flavor — returns each entry of
// a list-shaped argument as a string. Used by nodes(ids:) to recover
// per-id prefixes.
func rawArgValues(ctx context.Context, fc *graphql.FieldContext, name string) []string {
	if fc == nil || fc.Field.Field == nil {
		return nil
	}
	for _, a := range fc.Field.Arguments {
		if a.Name != name {
			continue
		}
		if a.Value == nil {
			return nil
		}
		raw, err := a.Value.Value(graphql.GetOperationContext(ctx).Variables)
		if err != nil {
			return nil
		}
		// Two raw shapes are possible:
		//   - `[]any` of strings when the document inlined a list
		//     literal: `nodes(ids: ["recording-1", "show-2"])`.
		//   - `[]string` when the document used a variable typed
		//     `[ID!]!` and the JSON deserializer produced a string
		//     slice directly.
		switch v := raw.(type) {
		case []any:
			out := make([]string, 0, len(v))
			for _, e := range v {
				if s, ok := e.(string); ok {
					out = append(out, s)
				}
			}
			return out
		case []string:
			return v
		}
	}
	return nil
}

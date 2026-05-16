package server

// listing.go — shared paging envelope for the remaining list-shaped
// JSON endpoints. After the GraphQL cutover the only REST list
// surface still using this shape is /api/v1/history; everything else
// (recordings, shows, people, wants) reads through /graphql with its
// own per-resolver pagination types.

// pageEnvelope wraps a list response in the {items, total, limit,
// offset} shape the SPA's Pagination component reads.
func pageEnvelope(items any, total, limit, offset int) map[string]any {
	return map[string]any{
		itemsKey: items,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	}
}

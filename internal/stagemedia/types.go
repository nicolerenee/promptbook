package stagemedia

// Images is the response payload for GET /api/images.
//
// Posters is the list of fully-qualified poster URLs for the show.
// Performers echoes back only the actor ids stagemedia has a headshot for —
// ids that were requested but not found are silently dropped, not returned
// with a null URL. Callers that care about per-id presence should diff the
// requested-vs-returned id sets.
//
// Error is null on a 200 success. On 4xx responses the API returns the same
// envelope with a short string (e.g. "Invalid show_id", "No actors") in
// Error and empty arrays in Posters/Performers. We model it as *string so
// that the JSON-null case round-trips cleanly: nil means "no error" and a
// non-nil empty string is an unexpected (but tolerated) shape.
type Images struct {
	Posters    []string    `json:"posters"`
	Performers []Performer `json:"performers"`
	Error      *string     `json:"error"`
}

// Performer is a stagemedia headshot for an Encora performer id.
type Performer struct {
	ID  int64  `json:"id"`
	URL string `json:"url"`
}

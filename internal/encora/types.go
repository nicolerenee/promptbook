package encora

// Recording mirrors a single Encora recording document. The shape matches
// what /api/recording/{id} and collection.data[i].recording return — they are
// byte-identical (verified during Phase 0 recon).
type Recording struct {
	ID            int64         `json:"id"`
	Show          string        `json:"show"`
	Tour          string        `json:"tour"`
	Date          Date          `json:"date"`
	Master        string        `json:"master"`
	NFT           NFT           `json:"nft"`
	Cast          []CastEntry   `json:"cast"`
	Notes         string        `json:"notes"`
	MasterNotes   string        `json:"master_notes"`
	ReleaseFormat *string       `json:"release_format"`
	Metadata      RecordingMeta `json:"metadata"`
}

// Date encodes Encora's partial-date model.
//
// FullDate is always ISO; consumers must check MonthKnown/DayKnown to format.
// Time is one of "evening", "matinee", "unknown". DateVariant disambiguates
// multiple recordings on the same date (Encora returns it as a string —
// observed values: "1", "4" — null otherwise).
type Date struct {
	FullDate    string  `json:"full_date"`
	MonthKnown  bool    `json:"month_known"`
	DayKnown    bool    `json:"day_known"`
	DateVariant *string `json:"date_variant"`
	Time        string  `json:"time"`
}

// NFT marks Not-For-Trade gating.
type NFT struct {
	NFTDate    *string `json:"nft_date"`
	NFTForever bool    `json:"nft_forever"`
}

// CastEntry pairs a performer with a character + status (u/s, swing).
type CastEntry struct {
	Performer Performer   `json:"performer"`
	Character Character   `json:"character"`
	Status    *CastStatus `json:"status"`
}

// CastStatus is the principal/understudy/swing/etc. label on a cast entry.
//
// Distinct values observed in fixtures: Alternate (alt), Emergency Cover (e/c),
// Swing (s/w), Temporary Replacement (t/r), Understudy (u/s). Nil means
// principal.
type CastStatus struct {
	Label        string `json:"label"`
	Abbreviation string `json:"abbreviation"`
}

// Performer is an actor.
type Performer struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
	URL  string `json:"url"`
}

// Character is a role in a show.
type Character struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
	URL   string `json:"url"`
	Order int    `json:"order"`
}

// RecordingMeta groups secondary metadata fields.
type RecordingMeta struct {
	ShowID              int64  `json:"show_id"`
	IsOpening           bool   `json:"is_opening"`
	IsClosing           bool   `json:"is_closing"`
	IsPreview           bool   `json:"is_preview"`
	IsConcert           bool   `json:"is_concert"`
	IsNFS               bool   `json:"is_nfs"`
	IsFavourite         bool   `json:"is_favourite"`
	Venue               string `json:"venue"`
	City                string `json:"city"`
	MediaType           string `json:"media_type"`
	RecordingType       string `json:"recording_type"`
	AmountRecorded      string `json:"amount_recorded"`
	GiftingStatus       string `json:"gifting_status"`
	LimitedStatus       string `json:"limited_status"`
	BootCampRecommended bool   `json:"boot_camp_recommended"`
	HasScreenshots      bool   `json:"has_screenshots"`
	HasSubtitles        bool   `json:"has_subtitles"`
	OwnersCount         int    `json:"owners_count"`
	WantersCount        int    `json:"wanters_count"`
	ShowDescription     string `json:"show_description"`
	LastUpdated         string `json:"last_updated"`
}

// CollectionEntry is one element of /api/collection.
type CollectionEntry struct {
	Recording   Recording `json:"recording"`
	Format      string    `json:"format"`
	Notes       *string   `json:"notes"`
	UserWatched int       `json:"user_watched"`
	UpdatedAt   string    `json:"updated_at"`
	CollectedAt string    `json:"collected_at"`
}

// WantEntry is one element of /api/wants.
//
// The wire format only carries the recording payload — no priority or added_at
// fields, despite the recon doc's earlier guess.
type WantEntry struct {
	Recording Recording `json:"recording"`
}

// Profile mirrors the /api/profile response.
//
// Only fields used by promptbook are mapped; the upstream payload carries
// extras (notifications, vouching, etc.) that we deliberately ignore.
type Profile struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	Slug              string `json:"slug"`
	Username          string `json:"username"`
	Status            string `json:"status"`
	RecordingsCount   int    `json:"recordings_count"`
	WantsCount        int    `json:"wants_count"`
	LastSeenAt        string `json:"last_seen_at"`
	ProfileVisibility string `json:"profile_visibility"`
	ColVisibility     string `json:"col_visibility"`
}

// Page is the Laravel-style pagination envelope.
type Page[T any] struct {
	Data        []T     `json:"data"`
	CurrentPage int     `json:"current_page"`
	LastPage    int     `json:"last_page"`
	PerPage     int     `json:"per_page"`
	Total       int     `json:"total"`
	From        int     `json:"from"`
	To          int     `json:"to"`
	NextPageURL *string `json:"next_page_url"`
	PrevPageURL *string `json:"prev_page_url"`
}

// Subtitle describes a subtitle asset.
type Subtitle struct {
	RecordingID int64  `json:"recording_id"`
	Language    string `json:"language"`
	Author      string `json:"author"`
	FileType    string `json:"file_type"`
	Coverage    string `json:"coverage"`
	URL         string `json:"url"`
}

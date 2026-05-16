package placeholder_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/placeholder"
)

func TestRenderHeadshot(t *testing.T) {
	t.Parallel()

	out, err := placeholder.Render(placeholder.KindHeadshot, 90001001, "Avery Morrison")
	require.NoError(t, err)
	body := string(out)

	assert.True(t, strings.HasPrefix(body, "<svg"),
		"output should start with <svg, got %q", body[:min(40, len(body))])
	// Headshot is a portrait 2:3 rectangle filled with the palette
	// color — no inner circle. Consumers (Jellyfin's portrait cast
	// card, our SPA) clip to a circle in CSS when they want the
	// circular visual.
	assert.NotContains(t, body, "<circle ",
		"portrait headshot should be a solid rectangle, not a circle")
	// Initials: first letter of each of the first two words: "A" + "M".
	assert.Contains(t, body, ">AM<", "expected initials AM in headshot text node")
	assert.Contains(t, body, `viewBox="0 0 400 600"`,
		"headshot viewBox should be 400x600 (2:3 portrait)")
}

func TestRenderShowBanner(t *testing.T) {
	t.Parallel()

	out, err := placeholder.Render(placeholder.KindShowBanner, 90004089, "Cresthaven")
	require.NoError(t, err)
	body := string(out)

	assert.True(t, strings.HasPrefix(body, "<svg"))
	assert.Contains(t, body, `viewBox="0 0 960 540"`, "banner is 16:9 at 960x540")
	assert.Contains(t, body, ">Cresthaven<", "banner label must be present")
	// Show banner uses a solid fill (no gradient stop reference).
	assert.NotContains(t, body, "url(#g)",
		"show banner should use a solid fill, not a gradient")
}

func TestRenderRecording_FanartMultiline(t *testing.T) {
	t.Parallel()

	label := "Cresthaven · OBC · 2016-08-12"
	out, err := placeholder.Render(placeholder.KindRecordingFanart, 90100222, label)
	require.NoError(t, err)
	body := string(out)

	assert.True(t, strings.HasPrefix(body, "<svg"))
	assert.Contains(t, body, `viewBox="0 0 1920 1080"`)
	// Each segment becomes its own tspan with the segment text.
	assert.Equal(t, 3, strings.Count(body, "<tspan "),
		"expected one tspan per ' · '-separated segment")
	assert.Contains(t, body, ">Cresthaven<")
	assert.Contains(t, body, ">OBC<")
	assert.Contains(t, body, ">2016-08-12<")
	// Recording fanart uses the gradient.
	assert.Contains(t, body, "url(#g)",
		"recording fanart should use the gradient fill")
}

func TestRenderRecording_PosterDimensions(t *testing.T) {
	t.Parallel()

	out, err := placeholder.Render(placeholder.KindRecordingPoster, 90100222, "Marigold · OBC")
	require.NoError(t, err)
	body := string(out)

	assert.Contains(t, body, `viewBox="0 0 600 900"`,
		"recording poster is 2:3 at 600x900")
	assert.Equal(t, 2, strings.Count(body, "<tspan "))
}

func TestPaletteStability(t *testing.T) {
	t.Parallel()

	a, err := placeholder.Render(placeholder.KindHeadshot, 42, "Alice Anderson")
	require.NoError(t, err)
	b, err := placeholder.Render(placeholder.KindHeadshot, 42, "Alice Anderson")
	require.NoError(t, err)
	assert.Equal(t, a, b, "same key + label must render identically")

	colorA := extractRectFill(t, string(a))
	c, err := placeholder.Render(placeholder.KindHeadshot, 999, "Alice Anderson")
	require.NoError(t, err)
	colorC := extractRectFill(t, string(c))
	assert.NotEqual(t, colorA, colorC,
		"different keys should usually pick different palette entries")
}

// extractRectFill returns the fill="..." attribute of the first
// <rect ...> element in the SVG. Test-only helper, naive on purpose.
func extractRectFill(t *testing.T, svg string) string {
	t.Helper()
	i := strings.Index(svg, "<rect")
	require.GreaterOrEqual(t, i, 0)
	rest := svg[i:]
	j := strings.Index(rest, `fill="`)
	require.GreaterOrEqual(t, j, 0)
	rest = rest[j+len(`fill="`):]
	k := strings.IndexByte(rest, '"')
	require.GreaterOrEqual(t, k, 0)
	return rest[:k]
}

func TestEmptyLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind placeholder.Kind
	}{
		{name: "headshot_question_mark", kind: placeholder.KindHeadshot},
		{name: "banner_question_mark", kind: placeholder.KindShowBanner},
		{name: "fanart_question_mark", kind: placeholder.KindRecordingFanart},
		{name: "poster_question_mark", kind: placeholder.KindRecordingPoster},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, err := placeholder.Render(tt.kind, 1, "")
			require.NoError(t, err)
			body := string(out)
			assert.True(t, strings.HasPrefix(body, "<svg"),
				"empty label must still produce a valid SVG")
			assert.Contains(t, body, ">?<",
				"empty label should fall through to '?' marker")
		})
	}
}

func TestVeryLongLabel(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("VeryLongShowName ", 30)
	out, err := placeholder.Render(placeholder.KindRecordingFanart, 5, long)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(out), "<svg"),
		"long label must not crash the renderer")
	assert.Contains(t, string(out), strings.TrimSpace(long))
}

func TestUnknownKindReturnsError(t *testing.T) {
	t.Parallel()

	_, err := placeholder.Render(placeholder.Kind(999), 1, "x")
	require.Error(t, err)
}

func TestNegativeKey(t *testing.T) {
	t.Parallel()

	out, err := placeholder.Render(placeholder.KindHeadshot, -7, "Bob")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(out), "<svg"))
}

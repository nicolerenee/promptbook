package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/rename"
)

// SubtitleFetcher knows how to download subtitle files for a recording
// next to its canonical video. Defined as an interface so ingest tests
// can pass a mock that doesn't actually hit Encora's CDN.
type SubtitleFetcher interface {
	Fetch(
		ctx context.Context,
		client Client,
		r encora.Recording,
		plan rename.Plan,
	) ([]string, error)
}

// HTTPSubtitleFetcher is the production fetcher: it pulls the subtitle
// list via the encora client and downloads each .url with the supplied
// http.Client. Real Encora calls are gated behind an explicit dependency
// — pass a mock here from tests.
type HTTPSubtitleFetcher struct {
	HTTP *http.Client
}

// Fetch implements SubtitleFetcher.
func (f *HTTPSubtitleFetcher) Fetch(
	ctx context.Context,
	client Client,
	r encora.Recording,
	plan rename.Plan,
) ([]string, error) {
	if f.HTTP == nil {
		return nil, errors.New("ingest: HTTPSubtitleFetcher.HTTP is nil")
	}
	subs, _, err := client.Subtitles(ctx, r.ID)
	if err != nil {
		return nil, fmt.Errorf("list subtitles: %w", err)
	}
	if len(subs) == 0 {
		return nil, nil
	}

	byLang := map[string]int{}
	for _, s := range subs {
		byLang[strings.ToLower(s.Language)]++
	}

	var written []string
	for _, sub := range subs {
		name := subtitleFilename(plan.TargetFile, sub, byLang[strings.ToLower(sub.Language)] > 1)
		dest := filepath.Join(plan.AbsoluteFolder(), name)
		if dlErr := f.downloadOne(ctx, sub.URL, dest); dlErr != nil {
			return written, fmt.Errorf("download %s: %w", sub.URL, dlErr)
		}
		written = append(written, dest)
	}
	return written, nil
}

func (f *HTTPSubtitleFetcher) downloadOne(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d for %s", resp.StatusCode, url)
	}

	const subtitleFilePerm = 0o644 // world-readable; jellyfin reads as a different uid
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, subtitleFilePerm)
	if err != nil {
		return fmt.Errorf("open dest: %w", err)
	}
	if _, copyErr := io.Copy(out, resp.Body); copyErr != nil {
		_ = out.Close()
		_ = os.Remove(dest)
		return fmt.Errorf("copy body: %w", copyErr)
	}
	return out.Close()
}

// subtitleFilename builds "<file>.<lang>.srt", or "<file>.<lang>.<author>.srt"
// when multiple subtitles share a language. Languages are lowercased and
// the .srt extension is forced (Encora hosts SRT exclusively in fixtures).
func subtitleFilename(stem string, sub encora.Subtitle, multipleForLang bool) string {
	lang := strings.ToLower(sub.Language)
	lang = strings.ReplaceAll(lang, " ", "_")
	parts := []string{stem, lang}
	if multipleForLang && sub.Author != "" {
		parts = append(parts, strings.ReplaceAll(sub.Author, " ", "_"))
	}
	return strings.Join(parts, ".") + ".srt"
}

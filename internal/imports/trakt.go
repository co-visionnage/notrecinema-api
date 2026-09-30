package imports

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,50}$`)

type traktIDs struct {
	Trakt int    `json:"trakt"`
	Imdb  string `json:"imdb"`
}

type traktShowOrMovie struct {
	IDs    traktIDs `json:"ids"`
	Title  string   `json:"title"`
	Year   int      `json:"year"`
	Genres []string `json:"genres"`
}

type traktWatchlistItem struct {
	Type  string            `json:"type"`
	Show  *traktShowOrMovie `json:"show"`
	Movie *traktShowOrMovie `json:"movie"`
}

// importTraktWatchlist — прямой перенос importTraktWatchlist: публичный
// watchlist читается одним только client id (без пользовательского OAuth).
// externalId -- IMDb id, если есть, иначе численный Trakt id как строка.
func (s *Service) importTraktWatchlist(ctx context.Context, username string) ([]ImportedSeries, error) {
	if s.traktClientID == "" {
		return nil, nil
	}
	if !usernamePattern.MatchString(username) {
		return nil, nil
	}

	reqURL := fmt.Sprintf("https://api.trakt.tv/users/%s/watchlist", url.PathEscape(username))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("trakt-api-version", "2")
	req.Header.Set("trakt-api-key", s.traktClientID)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}

	var items []traktWatchlistItem
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, nil
	}

	results := make([]ImportedSeries, 0, len(items))
	for _, item := range items {
		var entry *traktShowOrMovie
		if item.Type == "movie" {
			entry = item.Movie
		} else {
			entry = item.Show
		}
		if entry == nil {
			continue
		}

		externalID := entry.IDs.Imdb
		if externalID == "" {
			externalID = strconv.Itoa(entry.IDs.Trakt)
		}

		results = append(results, ImportedSeries{
			ExternalID: externalID,
			Source:     "trakt",
			Title:      entry.Title,
			Year:       entry.Year,
			Genres:     entry.Genres,
		})
	}
	return results, nil
}

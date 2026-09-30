package imports

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type omdbSearchItem struct {
	ImdbID string `json:"imdbID"`
}

type omdbSearchResponse struct {
	Search   []omdbSearchItem `json:"Search"`
	Response string           `json:"Response"`
}

type omdbDetailResponse struct {
	Title        string `json:"Title"`
	Year         string `json:"Year"`
	Genre        string `json:"Genre"`
	Poster       string `json:"Poster"`
	TotalSeasons string `json:"totalSeasons"`
	Response     string `json:"Response"`
}

// searchOmdb — прямой перенос searchOmdb: список (s=) + до 10 детальных
// запросов (i=<imdbID>) параллельно, т.к. поиск не отдаёт genres/poster/
// totalSeasons. currentYear используется, если OMDb не прислал валидный год.
func (s *Service) searchOmdb(ctx context.Context, query string) ([]ImportedSeries, error) {
	if s.omdbAPIKey == "" {
		return nil, nil
	}

	listURL := fmt.Sprintf("https://www.omdbapi.com/?apikey=%s&s=%s&type=series", s.omdbAPIKey, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}

	var list omdbSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil || list.Response != "True" {
		return nil, nil
	}

	items := list.Search
	if len(items) > 10 {
		items = items[:10]
	}

	results := make([]ImportedSeries, len(items))
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		go func(i int, imdbID string) {
			defer wg.Done()
			if series, ok := s.getOmdbByID(ctx, imdbID); ok {
				results[i] = series
			}
		}(i, item.ImdbID)
	}
	wg.Wait()

	out := make([]ImportedSeries, 0, len(results))
	for _, r := range results {
		if r.ExternalID != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Service) getOmdbByID(ctx context.Context, imdbID string) (ImportedSeries, bool) {
	if s.omdbAPIKey == "" {
		return ImportedSeries{}, false
	}

	detailURL := fmt.Sprintf("https://www.omdbapi.com/?apikey=%s&i=%s", s.omdbAPIKey, url.QueryEscape(imdbID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, detailURL, nil)
	if err != nil {
		return ImportedSeries{}, false
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return ImportedSeries{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ImportedSeries{}, false
	}

	var body omdbDetailResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Response != "True" {
		return ImportedSeries{}, false
	}

	year, yearErr := strconv.Atoi(body.Year)
	if yearErr != nil || year == 0 {
		year = time.Now().Year()
	}

	var genres []string
	if body.Genre != "" {
		for _, g := range strings.Split(body.Genre, ",") {
			genres = append(genres, strings.TrimSpace(g))
		}
	}

	imageURL := body.Poster
	if imageURL == "N/A" {
		imageURL = ""
	}

	var totalSeasons *int
	if body.TotalSeasons != "" {
		if n, err := strconv.Atoi(body.TotalSeasons); err == nil {
			totalSeasons = &n
		}
	}

	return ImportedSeries{
		ExternalID:   imdbID,
		Source:       "omdb",
		Title:        body.Title,
		Year:         year,
		Genres:       genres,
		ImageURL:     imageURL,
		TotalSeasons: totalSeasons,
	}, true
}

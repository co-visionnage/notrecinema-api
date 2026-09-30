package imports

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type kinopoiskDoc struct {
	ID              int    `json:"id"`
	Name            string `json:"name"`
	AlternativeName string `json:"alternativeName"`
	Year            int    `json:"year"`
	Genres          []struct {
		Name string `json:"name"`
	} `json:"genres"`
	Poster struct {
		URL        string `json:"url"`
		PreviewURL string `json:"previewUrl"`
	} `json:"poster"`
	SeasonsInfo []struct {
		EpisodesCount int `json:"episodesCount"`
	} `json:"seasonsInfo"`
}

type kinopoiskSearchResponse struct {
	Docs []kinopoiskDoc `json:"docs"`
}

func mapKinopoiskDoc(doc kinopoiskDoc) ImportedSeries {
	year := doc.Year
	if year == 0 {
		year = time.Now().Year()
	}

	title := doc.Name
	if title == "" {
		title = doc.AlternativeName
	}
	if title == "" {
		title = "Без названия"
	}

	genres := make([]string, 0, len(doc.Genres))
	for _, g := range doc.Genres {
		genres = append(genres, g.Name)
	}

	imageURL := doc.Poster.URL
	if imageURL == "" {
		imageURL = doc.Poster.PreviewURL
	}

	var totalSeasons, totalEpisodes *int
	if len(doc.SeasonsInfo) > 0 {
		n := len(doc.SeasonsInfo)
		totalSeasons = &n

		sum := 0
		for _, s := range doc.SeasonsInfo {
			sum += s.EpisodesCount
		}
		if sum > 0 {
			totalEpisodes = &sum
		}
	}

	return ImportedSeries{
		ExternalID:    strconv.Itoa(doc.ID),
		Source:        "kinopoisk",
		Title:         title,
		Year:          year,
		Genres:        genres,
		ImageURL:      imageURL,
		TotalSeasons:  totalSeasons,
		TotalEpisodes: totalEpisodes,
	}
}

// searchKinopoisk — прямой перенос searchKinopoisk: только сериалы
// (type=tv-series), лимит 10, новый api.kinopoisk.dev (не тот же API, что
// в internal/movieprovider).
func (s *Service) searchKinopoisk(ctx context.Context, query string) ([]ImportedSeries, error) {
	if s.kinopoiskAPIKey == "" {
		return nil, nil
	}

	u := url.URL{Scheme: "https", Host: "api.kinopoisk.dev", Path: "/v1.4/movie/search"}
	q := u.Query()
	q.Set("query", query)
	q.Set("limit", "10")
	q.Set("type", "tv-series")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-KEY", s.kinopoiskAPIKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}

	var body kinopoiskSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, nil
	}

	results := make([]ImportedSeries, 0, len(body.Docs))
	for _, doc := range body.Docs {
		results = append(results, mapKinopoiskDoc(doc))
	}
	return results, nil
}

package movieprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

const kinopoiskBaseURL = "https://kinopoiskapiunofficial.tech/api/v2.2/films"

type Kinopoisk struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

func NewKinopoisk(apiKey string, httpClient *http.Client) *Kinopoisk {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Kinopoisk{apiKey: apiKey, baseURL: kinopoiskBaseURL, httpClient: httpClient}
}

func (k *Kinopoisk) Name() string {
	return "kinopoisk"
}

type kinopoiskMovieResponse struct {
	KinopoiskID int    `json:"kinopoiskId"`
	NameRu      string `json:"nameRu"`
	NameEn      string `json:"nameEn"`
	Year        int    `json:"year"`
	PosterURL   string `json:"posterUrl"`
}

func (k *Kinopoisk) GetMovie(ctx context.Context, externalID string) (Movie, error) {
	url := fmt.Sprintf("%s/%s", k.baseURL, externalID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Movie{}, fmt.Errorf("kinopoisk: build request: %w", err)
	}
	req.Header.Set("X-API-KEY", k.apiKey)

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return Movie{}, fmt.Errorf("kinopoisk: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Movie{}, fmt.Errorf("kinopoisk: неожиданный статус %d", resp.StatusCode)
	}

	var body kinopoiskMovieResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Movie{}, fmt.Errorf("kinopoisk: decode response: %w", err)
	}

	title := body.NameRu
	if title == "" {
		title = body.NameEn
	}

	return Movie{
		ID:        fmt.Sprintf("%d", body.KinopoiskID),
		Title:     title,
		Year:      body.Year,
		PosterURL: body.PosterURL,
		Source:    k.Name(),
	}, nil
}

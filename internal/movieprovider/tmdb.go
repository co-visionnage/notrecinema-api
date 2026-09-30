package movieprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const tmdbBaseURL = "https://api.themoviedb.org/3"

type TMDB struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

func NewTMDB(apiKey string, httpClient *http.Client) *TMDB {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &TMDB{apiKey: apiKey, baseURL: tmdbBaseURL, httpClient: httpClient}
}

func (t *TMDB) Name() string {
	return "tmdb"
}

type tmdbMovieResponse struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	ReleaseDate string `json:"release_date"`
	PosterPath  string `json:"poster_path"`
}

func (t *TMDB) GetMovie(ctx context.Context, externalID string) (Movie, error) {
	url := fmt.Sprintf("%s/movie/%s?api_key=%s", t.baseURL, externalID, t.apiKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Movie{}, fmt.Errorf("tmdb: build request: %w", err)
	}

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return Movie{}, fmt.Errorf("tmdb: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Movie{}, fmt.Errorf("tmdb: неожиданный статус %d", resp.StatusCode)
	}

	var body tmdbMovieResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Movie{}, fmt.Errorf("tmdb: decode response: %w", err)
	}

	year, _ := strconv.Atoi(strings.SplitN(body.ReleaseDate, "-", 2)[0])

	var posterURL string
	if body.PosterPath != "" {
		posterURL = "https://image.tmdb.org/t/p/w500" + body.PosterPath
	}

	return Movie{
		ID:        strconv.Itoa(body.ID),
		Title:     body.Title,
		Year:      year,
		PosterURL: posterURL,
		Source:    t.Name(),
	}, nil
}

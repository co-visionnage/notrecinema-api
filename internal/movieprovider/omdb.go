package movieprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"notrecinema/api/internal/telemetry"
)

const omdbBaseURL = "https://www.omdbapi.com"

type OMDB struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

func NewOMDB(apiKey string, httpClient *http.Client) *OMDB {
	if httpClient == nil {
		httpClient = telemetry.InstrumentedClient()
	}
	return &OMDB{apiKey: apiKey, baseURL: omdbBaseURL, httpClient: httpClient}
}

func (o *OMDB) Name() string {
	return "omdb"
}

type omdbMovieResponse struct {
	ImdbID   string `json:"imdbID"`
	Title    string `json:"Title"`
	Year     string `json:"Year"`
	Poster   string `json:"Poster"`
	Response string `json:"Response"`
	Error    string `json:"Error"`
}

func (o *OMDB) GetMovie(ctx context.Context, externalID string) (Movie, error) {
	url := fmt.Sprintf("%s/?i=%s&apikey=%s", o.baseURL, externalID, o.apiKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Movie{}, fmt.Errorf("omdb: build request: %w", err)
	}

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return Movie{}, fmt.Errorf("omdb: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Movie{}, fmt.Errorf("omdb: неожиданный статус %d", resp.StatusCode)
	}

	var body omdbMovieResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Movie{}, fmt.Errorf("omdb: decode response: %w", err)
	}

	// OMDB отвечает 200 OK даже когда фильм не найден -- ошибку кодирует
	// полем Response:"False", а не HTTP-статусом.
	if body.Response == "False" {
		return Movie{}, fmt.Errorf("omdb: %s", body.Error)
	}

	year, _ := strconv.Atoi(body.Year)
	poster := body.Poster
	if poster == "N/A" {
		poster = ""
	}

	return Movie{
		ID:        body.ImdbID,
		Title:     body.Title,
		Year:      year,
		PosterURL: poster,
		Source:    o.Name(),
	}, nil
}

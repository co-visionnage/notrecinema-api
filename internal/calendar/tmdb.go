package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

const tmdbBaseURL = "https://api.themoviedb.org/3"

type nextEpisode struct {
	AirDate string
	Label   string
}

type tmdbFindResponse struct {
	TVResults []struct {
		ID int `json:"id"`
	} `json:"tv_results"`
}

type tmdbTVResponse struct {
	NextEpisodeToAir *struct {
		AirDate       string `json:"air_date"`
		SeasonNumber  int    `json:"season_number"`
		EpisodeNumber int    `json:"episode_number"`
	} `json:"next_episode_to_air"`
}

// findNextEpisode ищет следующую дату выхода эпизода по IMDb id сериала
// через TMDB: сперва /find разрешает imdb id в внутренний tmdb id, затем
// /tv/{id} отдаёт next_episode_to_air. Прямой перенос
// notrecinema-app/src/shared/lib/nextEpisode/tmdb.ts. Возвращает
// (nil, nil), если у TMDB просто нет данных о следующем эпизоде -- это не
// ошибка, сериал может быть завершён или ещё не анонсирован.
func findNextEpisode(ctx context.Context, httpClient *http.Client, apiKey, imdbID string) (*nextEpisode, error) {
	tvID, err := findTVID(ctx, httpClient, apiKey, imdbID)
	if err != nil {
		return nil, err
	}
	if tvID == 0 {
		return nil, nil
	}

	url := fmt.Sprintf("%s/tv/%d?api_key=%s", tmdbBaseURL, tvID, apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("calendar: build tv request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calendar: tv request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("calendar: tmdb tv: неожиданный статус %d", resp.StatusCode)
	}

	var body tmdbTVResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("calendar: decode tv response: %w", err)
	}

	if body.NextEpisodeToAir == nil {
		return nil, nil
	}

	return &nextEpisode{
		AirDate: body.NextEpisodeToAir.AirDate,
		Label:   fmt.Sprintf("S%dE%d", body.NextEpisodeToAir.SeasonNumber, body.NextEpisodeToAir.EpisodeNumber),
	}, nil
}

func findTVID(ctx context.Context, httpClient *http.Client, apiKey, imdbID string) (int, error) {
	url := fmt.Sprintf("%s/find/%s?external_source=imdb_id&api_key=%s", tmdbBaseURL, imdbID, apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("calendar: build find request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("calendar: find request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("calendar: tmdb find: неожиданный статус %d", resp.StatusCode)
	}

	var body tmdbFindResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("calendar: decode find response: %w", err)
	}

	if len(body.TVResults) == 0 {
		return 0, nil
	}
	return body.TVResults[0].ID, nil
}

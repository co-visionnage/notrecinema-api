package seasons

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

type seasonInfo struct {
	TotalSeasons  int
	TotalEpisodes *int // nil, если источник не даёт разбивку по эпизодам (OMDb)
}

// fetchCurrentSeasonInfo -- прямой перенос fetchCurrentSeasonInfo из
// notrecinema-app (src/shared/lib/importSeries/checkUpdates.ts): source,
// для которого нет обработчика (например 'imdb-csv', у которого просто нет
// стабильного API для повторного опроса), не ошибка -- просто "нет данных",
// как и в оригинале.
func fetchCurrentSeasonInfo(ctx context.Context, httpClient *http.Client, kinopoiskAPIKey, omdbAPIKey, source, externalID string) (*seasonInfo, error) {
	switch source {
	case "kinopoisk":
		return fetchKinopoiskSeasonInfo(ctx, httpClient, kinopoiskAPIKey, externalID)
	case "omdb":
		return fetchOMDbSeasonInfo(ctx, httpClient, omdbAPIKey, externalID)
	default:
		return nil, nil
	}
}

// api.kinopoisk.dev -- НЕ тот же Kinopoisk API, что уже используется в
// internal/movieprovider (kinopoiskapiunofficial.tech): именно этим
// (более новым) эндпоинтом и его полем seasonsInfo пользуется оригинал
// в notrecinema-app для подсчёта сезонов/эпизодов, у старого API такой
// разбивки нет.
const kinopoiskDevBaseURL = "https://api.kinopoisk.dev/v1.4/movie"

type kinopoiskSeasonsResponse struct {
	SeasonsInfo []struct {
		EpisodesCount int `json:"episodesCount"`
	} `json:"seasonsInfo"`
}

func fetchKinopoiskSeasonInfo(ctx context.Context, httpClient *http.Client, apiKey, externalID string) (*seasonInfo, error) {
	if apiKey == "" {
		return nil, nil
	}

	url := fmt.Sprintf("%s/%s", kinopoiskDevBaseURL, externalID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("seasons: build kinopoisk request: %w", err)
	}
	req.Header.Set("X-API-KEY", apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("seasons: kinopoisk request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}

	var body kinopoiskSeasonsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("seasons: decode kinopoisk response: %w", err)
	}

	if len(body.SeasonsInfo) == 0 {
		return nil, nil
	}

	totalEpisodes := 0
	for _, s := range body.SeasonsInfo {
		totalEpisodes += s.EpisodesCount
	}

	return &seasonInfo{TotalSeasons: len(body.SeasonsInfo), TotalEpisodes: &totalEpisodes}, nil
}

type omdbSeasonsResponse struct {
	Response     string `json:"Response"`
	TotalSeasons string `json:"totalSeasons"`
}

func fetchOMDbSeasonInfo(ctx context.Context, httpClient *http.Client, apiKey, externalID string) (*seasonInfo, error) {
	if apiKey == "" {
		return nil, nil
	}

	url := fmt.Sprintf("https://www.omdbapi.com/?i=%s&apikey=%s", externalID, apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("seasons: build omdb request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("seasons: omdb request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}

	var body omdbSeasonsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("seasons: decode omdb response: %w", err)
	}
	if body.Response != "True" {
		return nil, nil
	}

	totalSeasons, err := strconv.Atoi(body.TotalSeasons)
	if err != nil || totalSeasons == 0 {
		return nil, nil
	}

	// OMDb не даёт разбивку по эпизодам на сезон -- totalEpisodes остаётся
	// nil, как и в оригинале (getOmdbById никогда не возвращает totalEpisodes
	// для этого источника).
	return &seasonInfo{TotalSeasons: totalSeasons}, nil
}

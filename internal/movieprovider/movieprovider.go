// Package movieprovider абстрагирует получение метаданных фильма/сериала от
// конкретного внешнего API. Причина существования абстракции -- в
// co-visionnage-app уже интегрированы TMDB, Kinopoisk и OMDB
// (src/shared/api/movies или аналог), и ни один из них не гарантирует
// аптайм; Provider позволяет обернуть любой из них в одинаковый слой
// отказоустойчивости (timeout, retry, circuit breaker, fallback), не
// дублируя эту логику в каждой интеграции.
package movieprovider

import "context"

type Movie struct {
	ID        string
	Title     string
	Year      int
	PosterURL string
	Source    string
}

// Provider — контракт, который реализует каждый внешний источник.
// Единственный метод специально узкий: у разных источников разные наборы
// полей и разная пагинация поиска, но "дать метаданные по внешнему ID" есть
// у всех.
type Provider interface {
	// Name — человекочитаемое имя источника для логов и метрик
	// (external_api_errors_total{provider=...} и т.п. из будущего шага
	// observability).
	Name() string
	GetMovie(ctx context.Context, externalID string) (Movie, error)
}

// Package imports реализует источники для массового добавления сериалов:
// поиск по Kinopoisk/OMDb, импорт watchlist с Trakt, разбор CSV-экспорта
// IMDb -- прямой перенос notrecinema-app
// (src/app/api/import/*, src/shared/lib/importSeries/*).
package imports

// ImportedSeries -- прямой аналог ImportedSeries из notrecinema-app.
type ImportedSeries struct {
	ExternalID    string   `json:"externalId"`
	Source        string   `json:"source"` // kinopoisk | omdb | trakt | imdb-csv
	Title         string   `json:"title"`
	Year          int      `json:"year"`
	Genres        []string `json:"genres"`
	ImageURL      string   `json:"imageUrl,omitempty"`
	TotalSeasons  *int     `json:"totalSeasons,omitempty"`
	TotalEpisodes *int     `json:"totalEpisodes,omitempty"`
}

package imports

import (
	"strconv"
	"strings"
	"time"
)

// parseCSVLine — прямой перенос parseCsvLine: минимальный RFC 4180 парсер
// одной строки (без учёта переносов внутри кавычек -- это уже сделано на
// уровне parseCSVRows).
func parseCSVLine(line string) []string {
	var fields []string
	var current strings.Builder
	inQuotes := false

	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		switch {
		case inQuotes:
			if ch == '"' {
				if i+1 < len(runes) && runes[i+1] == '"' {
					current.WriteRune('"')
					i++
				} else {
					inQuotes = false
				}
			} else {
				current.WriteRune(ch)
			}
		case ch == '"':
			inQuotes = true
		case ch == ',':
			fields = append(fields, current.String())
			current.Reset()
		default:
			current.WriteRune(ch)
		}
	}
	fields = append(fields, current.String())
	return fields
}

// parseCSVRows — прямой перенос parseCsvRows: разбивает на строки, не
// разрывая переносы строк внутри кавычек.
func parseCSVRows(text string) [][]string {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	var rows [][]string
	var current strings.Builder
	inQuotes := false

	for _, ch := range normalized {
		if ch == '"' {
			inQuotes = !inQuotes
		}
		if ch == '\n' && !inQuotes {
			if current.Len() > 0 {
				rows = append(rows, parseCSVLine(current.String()))
			}
			current.Reset()
		} else {
			current.WriteRune(ch)
		}
	}
	if current.Len() > 0 {
		rows = append(rows, parseCSVLine(current.String()))
	}
	return rows
}

// ParseIMDbWatchlistCSV — прямой перенос parseImdbWatchlistCsv: колонки
// ищутся по имени заголовка (регистронезависимо), а не по позиции --
// порядок колонок в экспорте IMDb не гарантирован. Строки без Const/Title
// отбрасываются молча, как и в оригинале.
func ParseIMDbWatchlistCSV(csvText string) []ImportedSeries {
	rows := parseCSVRows(csvText)
	if len(rows) < 2 {
		return nil
	}

	header := make([]string, len(rows[0]))
	for i, col := range rows[0] {
		header[i] = strings.ToLower(strings.TrimSpace(col))
	}
	indexOf := func(name string) int {
		for i, col := range header {
			if col == name {
				return i
			}
		}
		return -1
	}

	constIndex := indexOf("const")
	titleIndex := indexOf("title")
	yearIndex := indexOf("year")
	genresIndex := indexOf("genres")

	if constIndex == -1 || titleIndex == -1 {
		return nil
	}

	at := func(row []string, i int) string {
		if i < 0 || i >= len(row) {
			return ""
		}
		return row[i]
	}

	var results []ImportedSeries
	for _, row := range rows[1:] {
		externalID := strings.TrimSpace(at(row, constIndex))
		title := strings.TrimSpace(at(row, titleIndex))
		if externalID == "" || title == "" {
			continue
		}

		year, err := strconv.Atoi(strings.TrimSpace(at(row, yearIndex)))
		if err != nil {
			year = time.Now().Year()
		}

		var genres []string
		if raw := at(row, genresIndex); raw != "" {
			for _, g := range strings.Split(raw, ",") {
				g = strings.TrimSpace(g)
				if g != "" {
					genres = append(genres, g)
				}
			}
		}

		results = append(results, ImportedSeries{
			ExternalID: externalID,
			Source:     "imdb-csv",
			Title:      title,
			Year:       year,
			Genres:     genres,
		})
	}
	return results
}

package imports

import "testing"

func TestParseIMDbWatchlistCSV(t *testing.T) {
	csv := "Const,Your Rating,Date Rated,Title,URL,Title Type,IMDb Rating,Runtime (mins),Year,Genres\n" +
		`tt0903747,,"2020-01-01","Breaking Bad",url,tvSeries,9.5,49,2008,"Crime, Drama, Thriller"` + "\n" +
		`tt0944947,,,"Game of Thrones",url,tvSeries,9.2,57,2011,"Action, Adventure, Drama"` + "\n"

	results := ParseIMDbWatchlistCSV(csv)
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2: %+v", len(results), results)
	}

	if results[0].ExternalID != "tt0903747" || results[0].Title != "Breaking Bad" || results[0].Year != 2008 {
		t.Errorf("results[0] = %+v, unexpected values", results[0])
	}
	wantGenres := []string{"Crime", "Drama", "Thriller"}
	if len(results[0].Genres) != len(wantGenres) {
		t.Fatalf("results[0].Genres = %v, want %v", results[0].Genres, wantGenres)
	}
	for i, g := range wantGenres {
		if results[0].Genres[i] != g {
			t.Errorf("results[0].Genres[%d] = %q, want %q", i, results[0].Genres[i], g)
		}
	}
	if results[0].Source != "imdb-csv" {
		t.Errorf("Source = %q, want imdb-csv", results[0].Source)
	}
}

func TestParseIMDbWatchlistCSVMissingRequiredColumns(t *testing.T) {
	csv := "Foo,Bar\nbaz,qux\n"
	if results := ParseIMDbWatchlistCSV(csv); results != nil {
		t.Errorf("results = %+v, want nil (missing Const/Title columns)", results)
	}
}

func TestParseIMDbWatchlistCSVTooFewRows(t *testing.T) {
	csv := "Const,Title\n"
	if results := ParseIMDbWatchlistCSV(csv); results != nil {
		t.Errorf("results = %+v, want nil (no data rows)", results)
	}
}

func TestParseIMDbWatchlistCSVSkipsRowsMissingRequiredFields(t *testing.T) {
	csv := "Const,Title,Year\n" +
		",Missing Const,2020\n" +
		"tt0000001,,2020\n" +
		"tt0000002,Valid Row,2020\n"

	results := ParseIMDbWatchlistCSV(csv)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1: %+v", len(results), results)
	}
	if results[0].ExternalID != "tt0000002" {
		t.Errorf("ExternalID = %q, want tt0000002", results[0].ExternalID)
	}
}

func TestParseIMDbWatchlistCSVHandlesQuotedCommasAndEscapedQuotes(t *testing.T) {
	csv := "Const,Title,Genres\n" +
		`tt0000001,"Title, with a comma",Drama` + "\n" +
		`tt0000002,"Title with ""quotes""",Comedy` + "\n"

	results := ParseIMDbWatchlistCSV(csv)
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2: %+v", len(results), results)
	}
	if results[0].Title != "Title, with a comma" {
		t.Errorf("results[0].Title = %q, want %q", results[0].Title, "Title, with a comma")
	}
	if results[1].Title != `Title with "quotes"` {
		t.Errorf("results[1].Title = %q, want %q", results[1].Title, `Title with "quotes"`)
	}
}

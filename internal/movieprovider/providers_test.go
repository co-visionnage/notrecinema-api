package movieprovider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTMDBParsesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":603,"title":"Матрица","release_date":"1999-03-30","poster_path":"/poster.jpg"}`))
	}))
	defer server.Close()

	tmdb := NewTMDB("test-key", server.Client())
	tmdb.baseURL = server.URL

	movie, err := tmdb.GetMovie(context.Background(), "603")
	if err != nil {
		t.Fatalf("GetMovie() error = %v", err)
	}

	want := Movie{ID: "603", Title: "Матрица", Year: 1999, PosterURL: "https://image.tmdb.org/t/p/w500/poster.jpg", Source: "tmdb"}
	if movie != want {
		t.Errorf("GetMovie() = %+v, want %+v", movie, want)
	}
}

func TestTMDBNonOKStatusIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	tmdb := NewTMDB("test-key", server.Client())
	tmdb.baseURL = server.URL

	if _, err := tmdb.GetMovie(context.Background(), "missing"); err == nil {
		t.Fatal("GetMovie() succeeded, want error on 404")
	}
}

func TestOMDBParsesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"imdbID":"tt0133093","Title":"The Matrix","Year":"1999","Poster":"https://example.com/p.jpg","Response":"True"}`))
	}))
	defer server.Close()

	omdb := NewOMDB("test-key", server.Client())
	omdb.baseURL = server.URL

	movie, err := omdb.GetMovie(context.Background(), "tt0133093")
	if err != nil {
		t.Fatalf("GetMovie() error = %v", err)
	}

	want := Movie{ID: "tt0133093", Title: "The Matrix", Year: 1999, PosterURL: "https://example.com/p.jpg", Source: "omdb"}
	if movie != want {
		t.Errorf("GetMovie() = %+v, want %+v", movie, want)
	}
}

func TestOMDBNotFoundReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// OMDB отвечает 200 OK даже когда фильм не найден.
		_, _ = w.Write([]byte(`{"Response":"False","Error":"Incorrect IMDb ID."}`))
	}))
	defer server.Close()

	omdb := NewOMDB("test-key", server.Client())
	omdb.baseURL = server.URL

	if _, err := omdb.GetMovie(context.Background(), "bad-id"); err == nil {
		t.Fatal("GetMovie() succeeded, want error when Response=False")
	}
}

func TestKinopoiskParsesResponse(t *testing.T) {
	var gotAPIKey string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-API-KEY")
		_, _ = w.Write([]byte(`{"kinopoiskId":301,"nameRu":"Матрица","year":1999,"posterUrl":"https://example.com/kp.jpg"}`))
	}))
	defer server.Close()

	kp := NewKinopoisk("test-key", server.Client())
	kp.baseURL = server.URL

	movie, err := kp.GetMovie(context.Background(), "301")
	if err != nil {
		t.Fatalf("GetMovie() error = %v", err)
	}

	want := Movie{ID: "301", Title: "Матрица", Year: 1999, PosterURL: "https://example.com/kp.jpg", Source: "kinopoisk"}
	if movie != want {
		t.Errorf("GetMovie() = %+v, want %+v", movie, want)
	}
	if gotAPIKey != "test-key" {
		t.Errorf("X-API-KEY header = %q, want %q", gotAPIKey, "test-key")
	}
}

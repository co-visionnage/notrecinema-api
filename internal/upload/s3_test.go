package upload

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUploadSeriesImageSignsAndPUTs проверяет реальный PUT-запрос с
// настоящей SigV4-подписью против httptest-сервера (не мок самой функции
// подписи) -- сервер получает запрос и мы проверяем, что заголовки,
// которые ожидает S3-совместимое хранилище, реально присутствуют.
func TestUploadSeriesImageSignsAndPUTs(t *testing.T) {
	var gotMethod, gotAuth, gotACL, gotContentType string
	var gotBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotACL = r.Header.Get("X-Amz-Acl")
		gotContentType = r.Header.Get("Content-Type")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = buf
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	svc := NewService(Config{
		Endpoint:  server.URL,
		Region:    "us-east-1",
		AccessKey: "test-access-key",
		SecretKey: "test-secret-key",
		Bucket:    "test-bucket",
	}, server.Client())

	if !svc.Configured() {
		t.Fatal("Configured() = false, want true")
	}

	data := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3}
	url, err := svc.UploadSeriesImage(context.Background(), data, SniffedImage{Extension: "png", MimeType: "image/png"})
	if err != nil {
		t.Fatalf("UploadSeriesImage() error: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotAuth == "" || !strings.Contains(gotAuth, "AWS4-HMAC-SHA256") {
		t.Errorf("Authorization header = %q, want AWS4-HMAC-SHA256 signature", gotAuth)
	}
	if gotACL != "public-read" {
		t.Errorf("X-Amz-Acl = %q, want public-read", gotACL)
	}
	if gotContentType != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", gotContentType)
	}
	if len(gotBody) != len(data) {
		t.Errorf("uploaded body length = %d, want %d", len(gotBody), len(data))
	}

	wantPrefix := server.URL + "/test-bucket/series-images/"
	if !strings.Contains(url, wantPrefix) {
		t.Errorf("returned URL = %q, want prefix %q", url, wantPrefix)
	}
}

func TestUploadSeriesImageRejectsErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<Error>AccessDenied</Error>"))
	}))
	defer server.Close()

	svc := NewService(Config{
		Endpoint:  server.URL,
		Region:    "us-east-1",
		AccessKey: "k",
		SecretKey: "s",
		Bucket:    "b",
	}, server.Client())

	_, err := svc.UploadSeriesImage(context.Background(), []byte{1, 2, 3}, SniffedImage{Extension: "png", MimeType: "image/png"})
	if err == nil {
		t.Fatal("UploadSeriesImage() with 403 response succeeded, want error")
	}
}

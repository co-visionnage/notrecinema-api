// Package upload загружает постер сериала в S3-совместимое хранилище
// (включая MinIO) -- прямой перенос notrecinema-app
// (src/shared/api/storage/s3.ts). Без официального AWS SDK: та же ручная
// подпись AWS4-HMAC-SHA256, что и в оригинале, только на Go crypto/hmac
// вместо node:crypto.
package upload

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

const service = "s3"
const algorithm = "AWS4-HMAC-SHA256"

type Config struct {
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	Bucket    string
	PublicURL string
}

func (c Config) configured() bool {
	return c.Endpoint != "" && c.Region != "" && c.AccessKey != "" && c.SecretKey != "" && c.Bucket != ""
}

func (c Config) publicURL() string {
	if c.PublicURL != "" {
		return strings.TrimSuffix(c.PublicURL, "/")
	}
	return strings.TrimSuffix(c.Endpoint, "/") + "/" + c.Bucket
}

type Service struct {
	cfg        Config
	httpClient *http.Client
}

func NewService(cfg Config, httpClient *http.Client) *Service {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Service{cfg: cfg, httpClient: httpClient}
}

func (s *Service) Configured() bool {
	return s.cfg.configured()
}

func hashSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// UploadSeriesImage — прямой перенос uploadImageToStorage: ключ объекта
// вида "series-images/<unix-millis>-<uuid>.<ext>", подписанный PUT-запрос
// напрямую по SigV4 (без SDK), x-amz-acl: public-read -- тот же выбор, что
// и в оригинале (загрузка предполагается сразу публично читаемой).
func (s *Service) UploadSeriesImage(ctx context.Context, data []byte, image SniffedImage) (string, error) {
	if !s.Configured() {
		return "", fmt.Errorf("upload: storage is not configured")
	}

	objectKey := fmt.Sprintf("series-images/%d-%s.%s", time.Now().UnixMilli(), uuid.NewString(), image.Extension)

	endpoint := strings.TrimSuffix(s.cfg.Endpoint, "/")
	reqURL := fmt.Sprintf("%s/%s/%s", endpoint, s.cfg.Bucket, objectKey)

	requestDate := time.Now().UTC().Format("20060102T150405Z")
	dateStamp := requestDate[:8]
	payloadHash := hashSHA256(data)

	host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	canonicalURI := "/" + s.cfg.Bucket + "/" + objectKey
	signedHeaders := "content-type;host;x-amz-acl;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := fmt.Sprintf(
		"content-type:%s\nhost:%s\nx-amz-acl:public-read\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		image.MimeType, host, payloadHash, requestDate,
	)
	canonicalRequest := strings.Join([]string{
		http.MethodPut, canonicalURI, "", canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, s.cfg.Region, service)
	stringToSign := strings.Join([]string{
		algorithm, requestDate, credentialScope, hashSHA256([]byte(canonicalRequest)),
	}, "\n")

	signingKey := hmacSHA256(
		hmacSHA256(
			hmacSHA256(
				hmacSHA256([]byte("AWS4"+s.cfg.SecretKey), dateStamp),
				s.cfg.Region,
			),
			service,
		),
		"aws4_request",
	)
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	authorization := fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, s.cfg.AccessKey, credentialScope, signedHeaders, signature)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("upload: build request: %w", err)
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Type", image.MimeType)
	req.Header.Set("X-Amz-Acl", "public-read")
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("X-Amz-Date", requestDate)
	req.ContentLength = int64(len(data))

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload: storage upload failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		details, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("upload: storage upload failed (%d): %s", resp.StatusCode, string(details))
	}

	return s.cfg.publicURL() + "/" + objectKey, nil
}

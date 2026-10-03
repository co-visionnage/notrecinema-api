package notifications

import (
	"testing"
)

// Вектор посчитан независимо (Python: hmac + base64url) и продублирован в
// notrecinema-worker/internal/unsubscribe: это и есть проверка, что два
// независимых модуля подписывают и проверяют ссылки одинаково.
const (
	vectorSecret = "test-secret"
	vectorUser   = "11111111-1111-1111-1111-111111111111"
	vectorToken  = "MTExMTExMTEtMTExMS0xMTExLTExMTEtMTExMTExMTExMTExfHNlcmllc19hZGRlZA.34QkSEz_HIOldM-BIyzSm6ETHnQpkdg0R1OJfkY1rJk"
)

func TestSignUnsubscribeTokenMatchesSharedVector(t *testing.T) {
	if got := SignUnsubscribeToken(vectorSecret, vectorUser, "series_added"); got != vectorToken {
		t.Errorf("token = %q, want %q", got, vectorToken)
	}
}

func TestParseUnsubscribeTokenAcceptsSharedVector(t *testing.T) {
	svc := (&Service{}).WithUnsubscribeSecret(vectorSecret)

	userID, category, ok := svc.parseUnsubscribeToken(vectorToken)
	if !ok || userID != vectorUser || category != "series_added" {
		t.Errorf("parse = (%q, %q, %v)", userID, category, ok)
	}
}

func TestParseUnsubscribeTokenRejectsForgeries(t *testing.T) {
	svc := (&Service{}).WithUnsubscribeSecret(vectorSecret)

	cases := map[string]string{
		"empty":            "",
		"garbage":          "not-a-token",
		"no signature":     "MTExMTExMTEtMTExMS0xMTExLTExMTEtMTExMTExMTExMTExfHNlcmllc19hZGRlZA",
		"empty signature":  "MTExMTExMTEtMTExMS0xMTExLTExMTEtMTExMTExMTExMTExfHNlcmllc19hZGRlZA.",
		"wrong secret":     SignUnsubscribeToken("another-secret", vectorUser, "series_added"),
		"unknown category": SignUnsubscribeToken(vectorSecret, vectorUser, "everything"),
		"empty user":       SignUnsubscribeToken(vectorSecret, "", "series_added"),
		"truncated sig":    vectorToken[:len(vectorToken)-3],
		"extra dot part":   vectorToken + ".x",
	}
	for name, token := range cases {
		if _, _, ok := svc.parseUnsubscribeToken(token); ok {
			t.Errorf("%s: a forged token was accepted", name)
		}
	}

	// Подмена пользователя при сохранённой подписи не должна проходить.
	forged := SignUnsubscribeToken(vectorSecret, "22222222-2222-2222-2222-222222222222", "series_added")
	payload := forged[:len(forged)-len(forged[indexOfDot(forged):])]
	tampered := payload + vectorToken[indexOfDot(vectorToken):]
	if _, _, ok := svc.parseUnsubscribeToken(tampered); ok {
		t.Error("a token with a swapped payload and an old signature was accepted")
	}
}

func indexOfDot(s string) int {
	for i := range s {
		if s[i] == '.' {
			return i
		}
	}
	return len(s)
}

func TestCategoriesAreConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Categories {
		if seen[c.Key] {
			t.Errorf("duplicate category %q", c.Key)
		}
		seen[c.Key] = true
		if c.EmailDefault && !c.EmailSupported {
			t.Errorf("%s: email is on by default but not supported", c.Key)
		}
	}
	if len(Categories) != 9 {
		t.Errorf("categories = %d, want 9 (the CHECK constraint in migration 0040 lists the same set)", len(Categories))
	}
}

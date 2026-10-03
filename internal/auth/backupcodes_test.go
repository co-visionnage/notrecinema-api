package auth

import (
	"strings"
	"testing"
)

func TestGenerateBackupCodesAreUniqueAndWellFormed(t *testing.T) {
	codes, err := generateBackupCodes()
	if err != nil {
		t.Fatalf("generateBackupCodes() error: %v", err)
	}
	if len(codes) != backupCodeCount {
		t.Fatalf("codes = %d, want %d", len(codes), backupCodeCount)
	}

	seen := map[string]bool{}
	for _, code := range codes {
		if len(code) != backupCodeLength+1 || code[backupCodeLength/2] != '-' {
			t.Errorf("code %q is not in xxxxx-xxxxx form", code)
		}
		normalized, ok := normalizeBackupCode(code)
		if !ok {
			t.Errorf("a freshly generated code %q does not normalize", code)
		}
		if seen[normalized] {
			t.Errorf("duplicate code %q", code)
		}
		seen[normalized] = true
	}
}

func TestNormalizeBackupCodeAcceptsHumanInput(t *testing.T) {
	cases := map[string]string{
		"abcde-fghjk":     "abcdefghjk",
		"ABCDE-FGHJK":     "abcdefghjk",
		"  abcde fghjk  ": "abcdefghjk",
		"abcdefghjk":      "abcdefghjk",
	}
	for input, want := range cases {
		got, ok := normalizeBackupCode(input)
		if !ok || got != want {
			t.Errorf("normalizeBackupCode(%q) = (%q, %v), want (%q, true)", input, got, ok, want)
		}
	}
}

func TestNormalizeBackupCodeRejectsNonCodes(t *testing.T) {
	for _, input := range []string{
		"",
		"123456",                // TOTP-код, не резервный
		"abcde-fghj",            // слишком короткий
		"abcde-fghjkm",          // слишком длинный
		"abcde-fghj0",           // 0 не входит в алфавит
		"abcde-fghjl",           // l не входит в алфавит
		"abcde-fghj!",           // спецсимвол
		strings.Repeat("a", 50), // мусор
	} {
		if _, ok := normalizeBackupCode(input); ok {
			t.Errorf("normalizeBackupCode(%q) accepted a malformed code", input)
		}
	}
}

func TestIsTOTPFormat(t *testing.T) {
	for input, want := range map[string]bool{
		"123456":      true,
		" 123456 ":    true,
		"12345":       false,
		"1234567":     false,
		"12345a":      false,
		"abcde-fghjk": false,
		"":            false,
	} {
		if got := isTOTPFormat(input); got != want {
			t.Errorf("isTOTPFormat(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestHashBackupCodesProducesVerifiableHashes(t *testing.T) {
	codes, err := generateBackupCodes()
	if err != nil {
		t.Fatalf("generateBackupCodes() error: %v", err)
	}
	hashes, err := hashBackupCodes(codes)
	if err != nil {
		t.Fatalf("hashBackupCodes() error: %v", err)
	}
	if len(hashes) != len(codes) {
		t.Fatalf("hashes = %d, want %d", len(hashes), len(codes))
	}

	for i, code := range codes {
		normalized, _ := normalizeBackupCode(code)
		if !VerifyPassword(normalized, hashes[i]) {
			t.Errorf("hash %d does not verify its own code", i)
		}
		if VerifyPassword("zzzzzzzzzz", hashes[i]) {
			t.Errorf("hash %d verifies a different code", i)
		}
		if strings.Contains(hashes[i], normalized) {
			t.Errorf("hash %d contains the code in the clear", i)
		}
	}
}

func TestHashBackupCodesRejectsMalformedInput(t *testing.T) {
	if _, err := hashBackupCodes([]string{"not-a-code"}); err == nil {
		t.Error("hashBackupCodes() accepted a malformed code")
	}
}

package auth

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"notrecinema/api/internal/platform/apperror"
)

// Резервные коды 2FA: одноразовые «запасные пароли» на случай потери
// устройства с аутентификатором. Показываются человеку один раз при
// включении 2FA (и при перевыпуске); в БД только медленные хэши.
const (
	backupCodeCount = 10
	// 31 символ без похожих друг на друга i, l, o, 0, 1: код читают с
	// бумаги и переписывают руками.
	backupCodeAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	backupCodeLength   = 10
)

// generateBackupCodes создаёт набор кодов в виде для показа: "abcde-fghjk".
func generateBackupCodes() ([]string, error) {
	max := big.NewInt(int64(len(backupCodeAlphabet)))
	codes := make([]string, 0, backupCodeCount)

	for len(codes) < backupCodeCount {
		raw := make([]byte, backupCodeLength)
		for i := range raw {
			n, err := rand.Int(rand.Reader, max)
			if err != nil {
				return nil, fmt.Errorf("auth: generate backup code: %w", err)
			}
			raw[i] = backupCodeAlphabet[n.Int64()]
		}
		codes = append(codes, string(raw[:backupCodeLength/2])+"-"+string(raw[backupCodeLength/2:]))
	}
	return codes, nil
}

// normalizeBackupCode приводит введённый человеком код к каноническому виду
// (без дефиса и пробелов, строчными). false -- это не похоже на резервный код.
func normalizeBackupCode(input string) (string, bool) {
	normalized := strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(input)))
	if len(normalized) != backupCodeLength {
		return "", false
	}
	for _, r := range normalized {
		if !strings.ContainsRune(backupCodeAlphabet, r) {
			return "", false
		}
	}
	return normalized, true
}

// isTOTPFormat -- шесть цифр: то, что показывает приложение-аутентификатор.
func isTOTPFormat(code string) bool {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// hashBackupCodes считает scrypt-хэши кодов параллельно: десять
// последовательных хэшей заметно тормозили бы включение 2FA.
func hashBackupCodes(codes []string) ([]string, error) {
	hashes := make([]string, len(codes))
	errs := make([]error, len(codes))

	var wg sync.WaitGroup
	for i, code := range codes {
		normalized, ok := normalizeBackupCode(code)
		if !ok {
			return nil, fmt.Errorf("auth: malformed backup code at index %d", i)
		}
		wg.Add(1)
		go func(i int, normalized string) {
			defer wg.Done()
			hashes[i], errs[i] = HashPassword(normalized)
		}(i, normalized)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return hashes, nil
}

// consumeBackupCode сверяет введённый код со всеми неиспользованными кодами
// пользователя и, если один подошёл, атомарно его гасит. false -- ни один не
// подошёл либо этот код уже успел потратить параллельный вход.
func (s *Service) consumeBackupCode(ctx context.Context, userID, normalized string) (bool, error) {
	type stored struct{ id, hash string }
	var candidates []stored

	rows, err := s.db.Query(ctx, `
		SELECT id, code_hash FROM public.get_two_factor_backup_code_hashes($1)
	`, userID)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var c stored
		if err := rows.Scan(&c.id, &c.hash); err != nil {
			rows.Close()
			return false, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}

	for _, c := range candidates {
		if !VerifyPassword(normalized, c.hash) {
			continue
		}

		var remaining int
		if err := s.db.QueryRow(ctx, `SELECT public.use_two_factor_backup_code($1)`, c.id).Scan(&remaining); err != nil {
			return false, err
		}
		return remaining >= 0, nil
	}
	return false, nil
}

// checkSecondFactor проверяет введённый код как TOTP либо как резервный.
// Использованный резервный код гасится сразу, независимо от того, чем
// закончится остальная операция: код, который однажды ушёл в сеть, считается
// скомпрометированным.
func (s *Service) checkSecondFactor(ctx context.Context, userID string, secret *string, code string) (bool, error) {
	code = strings.TrimSpace(code)

	if isTOTPFormat(code) {
		return secret != nil && verifyTOTP(code, *secret), nil
	}

	normalized, ok := normalizeBackupCode(code)
	if !ok {
		return false, nil
	}
	used, err := s.consumeBackupCode(ctx, userID, normalized)
	if err != nil {
		return false, apperror.Internal("failed to verify backup code", err)
	}
	return used, nil
}

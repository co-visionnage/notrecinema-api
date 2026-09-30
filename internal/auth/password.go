package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/scrypt"
)

// Параметры и формат хэша должны совпадать байт-в-байт с
// notrecinema-app/src/shared/api/postgres/auth.ts (createPasswordHash):
// scryptSync(password, salt, 64) с дефолтными N/r/p Node (16384/8/1) --
// это ровно те же дефолты, что задаёт golang.org/x/crypto/scrypt при тех
// же параметрах. Формат строки -- "<32-hex-символа-соль>:<128-hex-символов-ключ>",
// иначе Go-сервис не сможет проверить пароли, заведённые через монолит (и
// наоборот).
const (
	scryptN      = 16384
	scryptR      = 8
	scryptP      = 1
	scryptKeyLen = 64
	saltLen      = 16
)

// HashPassword хэширует пароль в том же формате, что createPasswordHash в
// notrecinema-app -- используется при регистрации.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}

	key, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return "", fmt.Errorf("auth: derive key: %w", err)
	}

	return hex.EncodeToString(salt) + ":" + hex.EncodeToString(key), nil
}

// VerifyPassword — прямой перенос verifyPassword из notrecinema-app:
// разбирает "соль:ключ", пересчитывает scrypt с той же солью и сравнивает
// в constant-time, чтобы не утекало время сравнения.
func VerifyPassword(password, passwordHash string) bool {
	salt, stored, ok := strings.Cut(passwordHash, ":")
	if !ok || salt == "" || stored == "" {
		return false
	}

	saltBytes, err := hex.DecodeString(salt)
	if err != nil {
		return false
	}
	storedBytes, err := hex.DecodeString(stored)
	if err != nil {
		return false
	}

	key, err := scrypt.Key([]byte(password), saltBytes, scryptN, scryptR, scryptP, len(storedBytes))
	if err != nil {
		return false
	}

	return subtle.ConstantTimeCompare(key, storedBytes) == 1
}

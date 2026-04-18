package crypto

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ErrEncryptionDisabled is returned by keyring and pgcrypto-backed operations in the
// SQLite single-user build (no sensitive_keyring / no server-side encryption).
var ErrEncryptionDisabled = errors.New("encryption and sensitive keyring are not available in this build")

// NormalizeKeyringPassword trims and lowercases passphrases used for the sensitive keyring
// (owner master and visitor seats) so storage and unlock are case-insensitive.
func NormalizeKeyringPassword(password string) string {
	return strings.ToLower(strings.TrimSpace(password))
}

// DeriveUserKey returns a hex-encoded 32-byte key derived from password+pepper via Argon2id.
func DeriveUserKey(password, pepper string) string {
	return hex.EncodeToString(DeriveKey(NormalizeKeyringPassword(password), pepper))
}

// InitSensitiveKeyring is a no-op in this build (no keyring table).
func InitSensitiveKeyring(_ context.Context, _ *sql.DB, masterPassword, _ string) error {
	if masterPassword == "" {
		return fmt.Errorf("master password required")
	}
	return ErrEncryptionDisabled
}

// GetMasterPrivateDEK always returns empty in this build.
func GetMasterPrivateDEK(_ context.Context, _ *sql.DB, _, _ string) (string, error) {
	return "", nil
}

// EncryptPrivateValue is unavailable in this build.
func EncryptPrivateValue(_ context.Context, _ *sql.DB, _, _, _ string) ([]byte, error) {
	return nil, ErrEncryptionDisabled
}

// DecryptPrivateValue returns empty when no keyring exists.
func DecryptPrivateValue(_ context.Context, _ *sql.DB, _ string, _ []byte, _ string) (string, error) {
	return "", nil
}

// GetSensitiveDEK always returns empty in this build.
func GetSensitiveDEK(_ context.Context, _ *sql.DB, _, _ string) (string, error) {
	return "", nil
}

// CheckSensitiveMasterPassword always returns false in this build.
func CheckSensitiveMasterPassword(_ context.Context, _ *sql.DB, _, _ string) (bool, error) {
	return false, nil
}

// FindVisitorKeyringIDForPassword always returns ok=false in this build.
func FindVisitorKeyringIDForPassword(_ context.Context, _ *sql.DB, _, _ string) (keyringID int64, ok bool, err error) {
	return 0, false, nil
}

// CheckSensitiveVisitorSeatPassword always returns false in this build.
func CheckSensitiveVisitorSeatPassword(_ context.Context, _ *sql.DB, _, _ string) (bool, error) {
	return false, nil
}

// AddSensitiveKeyringSeatTx is unavailable in this build.
func AddSensitiveKeyringSeatTx(_ context.Context, _ *sql.Tx, _ *sql.DB, _, _, _ string) (int64, error) {
	return 0, ErrEncryptionDisabled
}

// AddSensitiveKeyringSeat is unavailable in this build.
func AddSensitiveKeyringSeat(_ context.Context, _ *sql.DB, _, _, _ string) (int64, error) {
	return 0, ErrEncryptionDisabled
}

// DeleteSensitiveKeyringSeat is unavailable in this build.
func DeleteSensitiveKeyringSeat(_ context.Context, _ *sql.DB, _, _, _ string) error {
	return ErrEncryptionDisabled
}

// DeleteAllVisitorKeyringSeats is a no-op in this build.
func DeleteAllVisitorKeyringSeats(_ context.Context, _ *sql.DB, _, _ string) (int64, error) {
	return 0, nil
}

// DeleteVisitorKeyringSeatByID is unavailable in this build.
func DeleteVisitorKeyringSeatByID(_ context.Context, _ *sql.DB, _ int64, _, _ string) error {
	return ErrEncryptionDisabled
}

// SensitiveKeyringSeatCount returns 0 in this build.
func SensitiveKeyringSeatCount(_ context.Context, _ *sql.DB) (int, error) {
	return 0, nil
}

// EncryptDocumentData stores reference document bytes as-is (no server-side encryption).
func EncryptDocumentData(_ context.Context, _ *sql.DB, _ string, data []byte, _ string) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

// DecryptDocumentData returns stored bytes as-is (no encryption layer in this build).
func DecryptDocumentData(_ context.Context, _ *sql.DB, _ string, encData []byte, _ string) ([]byte, error) {
	if len(encData) == 0 {
		return nil, nil
	}
	out := make([]byte, len(encData))
	copy(out, encData)
	return out, nil
}

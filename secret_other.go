//go:build !windows

package main

import "errors"

// Outside Windows there is no per-account key store to lean on without extra
// dependencies, so the token stays as text in settings.json, which is written
// readable by this user only (mode 0600).

func secretSupported() bool { return false }

func protectSecret(plain string) (string, error) { return plain, nil }

func unprotectSecret(string) (string, error) {
	return "", errors.New("this token was encrypted by Windows and can only be read there")
}

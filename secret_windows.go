//go:build windows

package main

import (
	"encoding/base64"
	"errors"
	"syscall"
	"unsafe"
)

// The saved token is encrypted with Windows' Data Protection API (DPAPI):
// the key comes from the signed-in Windows account, so only that account,
// on this computer, can read it back. Copying settings.json elsewhere, or
// opening it as another user, gives only the encrypted text.

var (
	crypt32         = syscall.NewLazyDLL("crypt32.dll")
	procProtect     = crypt32.NewProc("CryptProtectData")
	procUnprotect   = crypt32.NewProc("CryptUnprotectData")
	procLocalFree   = syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree")
	secretEntropy   = []byte("Speed Test: Cloudflare Radar token")
	errNoProtection = errors.New("could not encrypt the token with Windows Data Protection")
)

const cryptprotectUIForbidden = 0x1

type dataBlob struct {
	n uint32
	p *byte
}

func blobOf(b []byte) *dataBlob {
	if len(b) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{n: uint32(len(b)), p: &b[0]}
}

func (b *dataBlob) bytes() []byte {
	out := make([]byte, b.n)
	copy(out, unsafe.Slice(b.p, b.n))
	_, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(b.p)))
	return out
}

func secretSupported() bool { return procProtect.Find() == nil && procUnprotect.Find() == nil }

func protectSecret(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	if !secretSupported() {
		return "", errNoProtection
	}
	var out dataBlob
	r, _, err := procProtect.Call(uintptr(unsafe.Pointer(blobOf([]byte(plain)))), 0,
		uintptr(unsafe.Pointer(blobOf(secretEntropy))), 0, 0, cryptprotectUIForbidden,
		uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return "", errors.Join(errNoProtection, err)
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(out.bytes()), nil
}

func unprotectSecret(stored string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(stored[len(secretPrefix):])
	if err != nil {
		return "", err
	}
	var out dataBlob
	r, _, err := procUnprotect.Call(uintptr(unsafe.Pointer(blobOf(raw))), 0,
		uintptr(unsafe.Pointer(blobOf(secretEntropy))), 0, 0, cryptprotectUIForbidden,
		uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return "", err
	}
	return string(out.bytes()), nil
}

//go:build !windows

package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
)

const sealedMagic = "AWGS1"

func protect(b []byte) ([]byte, error) {
	if len(SecretKey) == 0 {
		if RequireKey {
			return nil, errors.New("хранилище ключей Android (Keystore) недоступно — ключи не сохранены")
		}
		return b, nil
	}
	g, err := gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	rand.Read(nonce)
	out := append([]byte(sealedMagic), nonce...)
	return g.Seal(out, nonce, b, nil), nil
}

func unprotect(b []byte) ([]byte, error) {
	if len(b) < len(sealedMagic) || string(b[:len(sealedMagic)]) != sealedMagic {
		return b, nil // plaintext JSON written without a key
	}
	if len(SecretKey) == 0 {
		return nil, errors.New("ключ шифрования недоступен")
	}
	g, err := gcm()
	if err != nil {
		return nil, err
	}
	b = b[len(sealedMagic):]
	if len(b) < g.NonceSize() {
		return nil, errors.New("файл ключей повреждён")
	}
	return g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], nil)
}

func gcm() (cipher.AEAD, error) {
	blk, err := aes.NewCipher(SecretKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

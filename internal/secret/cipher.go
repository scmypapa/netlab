package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
)

type Cipher struct{ cipher.AEAD }

func OpenFile(path string) (*Cipher, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		file, err := os.CreateTemp(filepath.Dir(path), ".key-")
		if err != nil {
			return nil, err
		}
		defer os.Remove(file.Name())
		key = make([]byte, 32)
		_, err = rand.Read(key)
		if err == nil {
			_, err = file.Write(key)
		}
		if err == nil {
			err = file.Sync()
		}
		err = errors.Join(err, file.Close())
		if err != nil {
			return nil, err
		}
		// Publish a complete key; concurrent controllers read the same winner.
		if err = os.Link(file.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		key, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("secret key must contain 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	return &Cipher{aead}, err
}

func (c *Cipher) Encrypt(value []byte, owner string) []byte {
	return c.Seal(nil, nil, value, []byte(owner))
}

func (c *Cipher) Decrypt(value []byte, owner string) ([]byte, error) {
	return c.Open(nil, nil, value, []byte(owner))
}

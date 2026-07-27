// matrix-nctalk - A Matrix–Nextcloud Talk puppeting bridge.
// Copyright (C) 2026 Don O'Neill
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package connector

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// credentialPrefix marks a stored credential as encrypted.
//
// Its real job is telling the two apart: a bridge upgraded from a version that
// stored app passwords in the clear has to keep reading those until it has
// rewritten them, and guessing from the shape of the string would be a poor way
// to decide whether to try.
const credentialPrefix = "nctalk:v1:"

// ErrNoCredentialKey means the bridge has no key to encrypt credentials with.
var ErrNoCredentialKey = errors.New("no network.credential_key is configured")

// ErrCredentialUndecryptable means a stored credential did not decrypt, which
// in practice means the key changed since it was written.
var ErrCredentialUndecryptable = errors.New("the stored Nextcloud credential could not be decrypted")

// credentialCipher builds the AEAD used for credentials at rest.
//
// The configured key is hashed rather than used directly so that any
// passphrase, of any length, yields a valid 32-byte key. It is not a password
// hash and is not meant to be: the value is generated random and full length,
// and stretching it would only slow down the bridge's own startup.
func credentialCipher(key string) (cipher.AEAD, error) {
	if key == "" {
		return nil, ErrNoCredentialKey
	}
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("build cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// isEncryptedCredential reports whether a stored value has already been through
// encryptCredential.
func isEncryptedCredential(stored string) bool {
	return strings.HasPrefix(stored, credentialPrefix)
}

// encryptCredential turns an app password into the form kept in the database.
func (nc *NCTalkConnector) encryptCredential(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	aead, err := credentialCipher(nc.Config.CredentialKey)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	// The nonce travels with the ciphertext; it is not secret, only unique.
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return credentialPrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// decryptCredential reads a credential out of the database.
//
// A value without the marker is returned unchanged: it was written before the
// bridge encrypted them, and refusing to read it would lock the user out of a
// login that still works.
func (nc *NCTalkConnector) decryptCredential(stored string) (string, error) {
	if stored == "" || !isEncryptedCredential(stored) {
		return stored, nil
	}

	aead, err := credentialCipher(nc.Config.CredentialKey)
	if err != nil {
		return "", err
	}

	sealed, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(stored, credentialPrefix))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrCredentialUndecryptable, err)
	}
	if len(sealed) < aead.NonceSize() {
		return "", fmt.Errorf("%w: too short to hold a nonce", ErrCredentialUndecryptable)
	}

	nonce, ciphertext := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// GCM authenticates, so this is the wrong key or a tampered row rather
		// than merely unreadable bytes.
		return "", ErrCredentialUndecryptable
	}
	return string(plaintext), nil
}

// newLoginMetadata builds the row a completed login is stored as.
//
// This exists as its own step because it is where the credential stops being a
// password and becomes a database value: keeping it out of finish() means the
// property that the two differ can be asserted without standing up a bridge.
func (nc *NCTalkConnector) newLoginMetadata(serverURL, username, appPassword string, features []string) (*UserLoginMetadata, error) {
	stored, err := nc.encryptCredential(appPassword)
	if err != nil {
		return nil, fmt.Errorf("could not encrypt the app password for storage: %w", err)
	}
	return &UserLoginMetadata{
		ServerURL:   serverURL,
		Username:    username,
		AppPassword: stored,
		Features:    features,
	}, nil
}

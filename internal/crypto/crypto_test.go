package crypto_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thiagoribeiro/nimlet/internal/crypto"
)

func TestPassword_HashAndVerify(t *testing.T) {
	hash, err := crypto.HashPassword("correct-horse-battery-staple")
	require.NoError(t, err)
	assert.NotEmpty(t, hash)

	ok, err := crypto.VerifyPassword("correct-horse-battery-staple", hash)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = crypto.VerifyPassword("wrong-password", hash)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestToken_RoundTrip(t *testing.T) {
	secret := []byte("test-secret-32-chars-minimum!!!")
	sessionID := "session-uuid-abc123"

	token, err := crypto.NewSessionToken(sessionID, secret)
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.Contains(t, token, ".")

	got, err := crypto.ParseSessionToken(token, secret)
	require.NoError(t, err)
	assert.Equal(t, sessionID, got)
}

func TestToken_TamperedSignatureRejected(t *testing.T) {
	secret := []byte("test-secret-32-chars-minimum!!!")
	token, _ := crypto.NewSessionToken("session123", secret)

	// Corrupt the signature part.
	_, err := crypto.ParseSessionToken(token+"x", secret)
	assert.Error(t, err)
}

func TestToken_WrongSecretRejected(t *testing.T) {
	token, _ := crypto.NewSessionToken("session123", []byte("secret-a-32-chars-minimum!!!!!!"))
	_, err := crypto.ParseSessionToken(token, []byte("secret-b-32-chars-minimum!!!!!!"))
	assert.Error(t, err)
}

func TestTokenHash_IsDeterministic(t *testing.T) {
	h1 := crypto.TokenHash("my-token")
	h2 := crypto.TokenHash("my-token")
	assert.Equal(t, h1, h2)
}

func TestAES_SealAndOpen(t *testing.T) {
	key := make([]byte, 32)
	enc, err := crypto.NewEncryptor(key)
	require.NoError(t, err)

	plaintext := []byte("hello nimlet")
	ciphertext, err := enc.Seal(plaintext)
	require.NoError(t, err)
	assert.NotEqual(t, plaintext, ciphertext)

	decrypted, err := enc.Open(ciphertext)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestAES_WrongKeyFails(t *testing.T) {
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	key2[0] = 1

	enc1, _ := crypto.NewEncryptor(key1)
	enc2, _ := crypto.NewEncryptor(key2)

	ciphertext, _ := enc1.Seal([]byte("secret"))
	_, err := enc2.Open(ciphertext)
	assert.Error(t, err)
}

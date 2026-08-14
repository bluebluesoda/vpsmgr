package pw

import (
	"crypto/rand"
	"math/big"

	"golang.org/x/crypto/bcrypt"
)

const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// urlCharset is [a-zA-Z0-9_-], URL-safe without percent-encoding.
const urlCharset = charset + "-_"

func randomFrom(cs string, n int) string {
	out := make([]byte, n)
	idx := big.NewInt(int64(len(cs)))
	for i := range out {
		r, err := rand.Int(rand.Reader, idx)
		if err != nil {
			// crypto/rand failure is unrecoverable in practice; fall back to time seed
			panic(err)
		}
		out[i] = cs[r.Int64()]
	}
	return string(out)
}

// Generate returns a random n-char password from [a-zA-Z0-9].
func Generate(n int) string { return randomFrom(charset, n) }

// URLSafe returns a random n-char string from [a-zA-Z0-9_-] (URL-safe).
func URLSafe(n int) string { return randomFrom(urlCharset, n) }

func Hash(p string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	return string(b), err
}

func Verify(hash, p string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(p)) == nil
}

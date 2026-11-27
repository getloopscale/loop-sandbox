package secretbox_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/secretbox"
)

var (
	key      = []byte("0123456789abcdef0123456789abcdef")
	otherKey = []byte("fedcba9876543210fedcba9876543210")
)

func box(t *testing.T, k []byte) *secretbox.Box {
	t.Helper()
	b, err := secretbox.New(k)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func sealed(t *testing.T, b *secretbox.Box, plaintext, associated string) []byte {
	t.Helper()
	out, err := b.Seal([]byte(plaintext), []byte(associated))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return out
}

func TestOpenReturnsWhatWasSealed(t *testing.T) {
	b := box(t, key)
	ciphertext := sealed(t, b, "s3cret-value", "tenant-a/TOKEN")

	got, err := b.Open(ciphertext, []byte("tenant-a/TOKEN"))

	if err != nil || string(got) != "s3cret-value" {
		t.Errorf("Open = %q, %v; want s3cret-value", got, err)
	}
}

func TestTheCiphertextDoesNotContainThePlaintext(t *testing.T) {
	b := box(t, key)

	ciphertext := sealed(t, b, "s3cret-value", "tenant-a/TOKEN")

	if bytes.Contains(ciphertext, []byte("s3cret-value")) {
		t.Error("the ciphertext contains the plaintext")
	}
}

func TestSealingTheSameValueTwiceGivesDifferentBytes(t *testing.T) {
	b := box(t, key)

	first, second := sealed(t, b, "v", "a"), sealed(t, b, "v", "a")

	if bytes.Equal(first, second) {
		t.Error("two seals of one value are equal, so the nonce is not random")
	}
}

func TestOpenRefusesACiphertextItCannotAuthenticate(t *testing.T) {
	b := box(t, key)
	ciphertext := sealed(t, b, "s3cret-value", "tenant-a/TOKEN")
	altered := append([]byte(nil), ciphertext...)
	altered[len(altered)-1] ^= 1
	cases := map[string]struct {
		box        *secretbox.Box
		sealed     []byte
		associated string
	}{
		"other associated data": {b, ciphertext, "tenant-b/TOKEN"},
		"another key":           {box(t, otherKey), ciphertext, "tenant-a/TOKEN"},
		"an altered byte":       {b, altered, "tenant-a/TOKEN"},
		"cut short":             {b, ciphertext[:10], "tenant-a/TOKEN"},
		"empty":                 {b, nil, "tenant-a/TOKEN"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := c.box.Open(c.sealed, []byte(c.associated))

			if !errors.Is(err, secretbox.ErrOpen) || got != nil {
				t.Errorf("Open = %q, %v; want nil and ErrOpen", got, err)
			}
		})
	}
}

func TestNewRefusesAKeyOfTheWrongLength(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := secretbox.New(make([]byte, n)); err == nil {
			t.Errorf("New accepted a key of %d bytes", n)
		}
	}
}

func TestKeyIDIsStableForAKeyAndDiffersBetweenKeys(t *testing.T) {
	a, again, other := box(t, key), box(t, key), box(t, otherKey)

	if a.KeyID() != again.KeyID() || a.KeyID() == other.KeyID() || a.KeyID() == "" {
		t.Errorf("key ids: %q, %q, %q; want the first two equal and the third different", a.KeyID(), again.KeyID(), other.KeyID())
	}
}

package signedurl_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/signedurl"
)

var (
	key = []byte("0123456789abcdef0123456789abcdef")
	now = time.Unix(1_700_000_000, 0).UTC()
)

func signer(t *testing.T, k []byte) *signedurl.Signer {
	t.Helper()
	s, err := signedurl.New(k)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func claims() signedurl.Claims {
	return signedurl.Claims{TenantID: "tnt_1", SandboxID: "sbx_1", Path: "/workspace/a.txt",
		Operation: signedurl.OpUpload, ExpiresAt: now.Add(time.Minute)}
}

func TestVerifyReturnsTheClaimsThatWereSigned(t *testing.T) {
	s := signer(t, key)
	token, err := s.Sign(claims())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	got, err := s.Verify(token, now)

	want := claims()
	if err != nil || got.TenantID != want.TenantID || got.SandboxID != want.SandboxID || got.Path != want.Path ||
		got.Operation != want.Operation || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("claims = %+v err = %v, want %+v", got, err, want)
	}
}

func TestVerifyRefusesATokenAtOrAfterItsExpiry(t *testing.T) {
	s := signer(t, key)
	token, _ := s.Sign(claims())
	for name, at := range map[string]time.Time{"at the expiry": now.Add(time.Minute), "after it": now.Add(time.Hour)} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Verify(token, at); !errors.Is(err, signedurl.ErrExpired) {
				t.Errorf("err = %v, want ErrExpired", err)
			}
		})
	}
}

func TestVerifyRefusesATokenSignedWithAnotherKey(t *testing.T) {
	token, _ := signer(t, []byte("another-key-another-key-another-k")).Sign(claims())

	_, err := signer(t, key).Verify(token, now)

	if !errors.Is(err, signedurl.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestVerifyRefusesATamperedToken(t *testing.T) {
	s := signer(t, key)
	token, _ := s.Sign(claims())
	payload, sig, _ := strings.Cut(token, ".")
	other := claims()
	other.Path = "/workspace/secret.txt"
	otherToken, _ := s.Sign(other)
	otherPayload, _, _ := strings.Cut(otherToken, ".")
	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"t":"tnt_2","s":"sbx_1","p":"/workspace/a.txt","o":"upload","e":9999999999}`))
	cases := map[string]string{
		"another payload with this signature": otherPayload + "." + sig,
		"a forged payload":                    forged + "." + sig,
		"a truncated signature":               payload + "." + sig[:len(sig)-2],
		"no signature":                        payload,
		"an empty signature":                  payload + ".",
		"empty":                               "",
		"not base64":                          "%%%.%%%",
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Verify(tok, now); !errors.Is(err, signedurl.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestSignRefusesAnUnknownOperation(t *testing.T) {
	c := claims()
	c.Operation = "delete"

	if _, err := signer(t, key).Sign(c); err == nil {
		t.Error("an unknown operation was signed")
	}
}

func TestNewRefusesAShortKey(t *testing.T) {
	if _, err := signedurl.New([]byte("short")); err == nil {
		t.Error("a short key was accepted")
	}
}

// The last character of a 43-character signature carries four bits of data and
// two that must be zero. A decoder that ignores the two would accept four
// spellings of one token.
func TestVerifyRefusesASignatureWithNonCanonicalTrailingBits(t *testing.T) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	s := signer(t, key)
	token, err := s.Sign(claims())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	last := strings.IndexByte(alphabet, token[len(token)-1])
	respelled := token[:len(token)-1] + string(alphabet[last+1]) // same four data bits, one padding bit set

	_, err = s.Verify(respelled, now)

	if !errors.Is(err, signedurl.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid for a second spelling of the same signature", err)
	}
}

package cursor

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func codec(t *testing.T, secret string) *Codec {
	t.Helper()
	c, err := New([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRoundTrip(t *testing.T) {
	c := codec(t, "secret-at-least-32-characters-long!!")
	for _, pos := range []int64{0, 1, 1500, 1 << 40} {
		tok := c.Encode(Sync, "user-a", pos)
		got, err := c.Decode(Sync, "user-a", tok)
		if err != nil || got != pos {
			t.Fatalf("pos %d: got %d, %v", pos, got, err)
		}
		if strings.Contains(tok, strconv.FormatInt(pos, 10)) && pos > 9 {
			t.Fatalf("token %q exposes the position", tok)
		}
	}
	if a, b := c.Encode(Sync, "u", 7), c.Encode(Sync, "u", 7); a == b {
		t.Fatal("tokens for the same position must differ (random nonce): equal tokens leak equality")
	}
	if pos, err := c.Decode(Sync, "u", ""); err != nil || pos != 0 {
		t.Fatalf("empty token = start: got %d, %v", pos, err)
	}
}

func TestRejects(t *testing.T) {
	c := codec(t, "secret-at-least-32-characters-long!!")
	other := codec(t, "another-secret-at-least-32-chars!!!")
	tok := c.Encode(Sync, "user-a", 42)

	flipped := []byte(tok)
	flipped[len(flipped)-3] ^= 1

	cases := map[string]func() error{
		"another user":    func() error { _, err := c.Decode(Sync, "user-b", tok); return err },
		"another kind":    func() error { _, err := c.Decode(Kind('p'), "user-a", tok); return err },
		"another key":     func() error { _, err := other.Decode(Sync, "user-a", tok); return err },
		"tampered":        func() error { _, err := c.Decode(Sync, "user-a", string(flipped)); return err },
		"numeric legacy":  func() error { _, err := c.Decode(Sync, "user-a", "1500"); return err },
		"unknown version": func() error { _, err := c.Decode(Sync, "user-a", "c9."+tok[3:]); return err },
		"garbage":         func() error { _, err := c.Decode(Sync, "user-a", "c1.!!!"); return err },
		"truncated":       func() error { _, err := c.Decode(Sync, "user-a", "c1.AAAA"); return err },
	}
	for name, decode := range cases {
		if err := decode(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func BenchmarkEncodeDecode(b *testing.B) {
	c, _ := New([]byte("secret-at-least-32-characters-long!!"))
	for b.Loop() {
		tok := c.Encode(Sync, "7f6c1b2e-0d1f-4a8e-9b1a-3c2d4e5f6a7b", 123456789)
		if _, err := c.Decode(Sync, "7f6c1b2e-0d1f-4a8e-9b1a-3c2d4e5f6a7b", tok); err != nil {
			b.Fatal(err)
		}
	}
}

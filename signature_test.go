package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testHMACSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var sigNow = time.Unix(1700000000, 0)

func signedServer(t *testing.T) *server {
	t.Helper()
	s := newTestServer(t)
	s.hmacSecret = []byte(testHMACSecret)
	old := timeNow
	timeNow = func() time.Time { return sigNow }
	t.Cleanup(func() { timeNow = old })
	return s
}

// signHeaders builds the headers the Pebble app sends, from the spec's
// signing input rather than from the code under test.
func signHeaders(secret string, ts int64, delivery, trigger string, test bool, body []byte) map[string]string {
	flag := "0"
	if test {
		flag = "1"
	}
	prefix := fmt.Sprintf("v1\n%d\n%s\n%s\n%s\n", ts, delivery, trigger, flag)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(prefix))
	m.Write(body)
	h := map[string]string{
		"X-Index-Webhook-Version": "1",
		"X-Index-Signature":       hex.EncodeToString(m.Sum(nil)),
		"X-Index-Timestamp":       strconv.FormatInt(ts, 10),
		"X-Index-Delivery":        delivery,
		"X-Index-Trigger":         trigger,
	}
	if test {
		h["X-Index-Test"] = "true"
	}
	return h
}

func noteBody(t *testing.T) ([]byte, string) {
	t.Helper()
	r, ct := multipartBody(t, map[string]string{"transcription": "buy milk"}, "", "")
	b, _ := io.ReadAll(r)
	return b, ct
}

func TestSignatureKnownVector(t *testing.T) {
	h := signHeaders(testHMACSecret, 1700000000, "delivery-1", "single-click-hold", false, []byte("hello-body"))
	want := "082dce3ecad57cff72c795dceb0d3b1b837791fd8f2a282132bfe7c205c1ed1c" // computed with Python's hmac
	if h["X-Index-Signature"] != want {
		t.Fatalf("test helper disagrees with the independent vector: %s", h["X-Index-Signature"])
	}
}

func TestSignatureAccepted(t *testing.T) {
	for _, test := range []bool{false, true} {
		s := signedServer(t)
		body, ct := noteBody(t)
		h := signHeaders(testHMACSecret, sigNow.Unix(), "d-1", "double-click-hold", test, body)
		w := doNote(t, s, bytes.NewReader(body), ct, h)
		if w.Code != http.StatusOK {
			t.Fatalf("test=%v: got %d, want 200", test, w.Code)
		}
		readNote(t, s.vault)
	}
}

func TestSignatureAcceptsEpilogueAndWindowEdge(t *testing.T) {
	s := signedServer(t)
	body, ct := noteBody(t)
	body = append(body, []byte("trailing epilogue the multipart reader ignores\r\n")...)
	h := signHeaders(testHMACSecret, sigNow.Unix()-300, "d-2", "single-click-hold", false, body)
	if w := doNote(t, s, bytes.NewReader(body), ct, h); w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
}

func TestSignatureRejected(t *testing.T) {
	body, ct := noteBody(t)
	good := func() map[string]string {
		return signHeaders(testHMACSecret, sigNow.Unix(), "d-3", "single-click-hold", false, body)
	}
	cases := map[string]func(h map[string]string) []byte{
		"no signature headers": func(h map[string]string) []byte {
			for k := range h {
				delete(h, k)
			}
			return body
		},
		"missing signature": func(h map[string]string) []byte { delete(h, "X-Index-Signature"); return body },
		"missing timestamp": func(h map[string]string) []byte { delete(h, "X-Index-Timestamp"); return body },
		"missing delivery":  func(h map[string]string) []byte { delete(h, "X-Index-Delivery"); return body },
		"wrong version":     func(h map[string]string) []byte { h["X-Index-Webhook-Version"] = "2"; return body },
		"bad hex":           func(h map[string]string) []byte { h["X-Index-Signature"] = strings.Repeat("z", 64); return body },
		"short signature":   func(h map[string]string) []byte { h["X-Index-Signature"] = "abcd"; return body },
		"uppercase hex": func(h map[string]string) []byte {
			h["X-Index-Signature"] = strings.ToUpper(h["X-Index-Signature"])
			return body
		},
		"wrong secret": func(h map[string]string) []byte {
			g := signHeaders("x"+testHMACSecret[1:], sigNow.Unix(), "d-3", "single-click-hold", false, body)
			h["X-Index-Signature"] = g["X-Index-Signature"]
			return body
		},
		"stale timestamp": func(h map[string]string) []byte {
			g := signHeaders(testHMACSecret, sigNow.Unix()-301, "d-3", "single-click-hold", false, body)
			for k, v := range g {
				h[k] = v
			}
			return body
		},
		"future timestamp": func(h map[string]string) []byte {
			g := signHeaders(testHMACSecret, sigNow.Unix()+301, "d-3", "single-click-hold", false, body)
			for k, v := range g {
				h[k] = v
			}
			return body
		},
		"tampered body": func(h map[string]string) []byte {
			return bytes.Replace(body, []byte("buy milk"), []byte("buy beer"), 1)
		},
		"trigger swapped":   func(h map[string]string) []byte { h["X-Index-Trigger"] = "double-click-hold"; return body },
		"test flag flipped": func(h map[string]string) []byte { h["X-Index-Test"] = "true"; return body },
		"truncated body":    func(h map[string]string) []byte { return body[:len(body)/2] },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := signedServer(t)
			h := good()
			b := mutate(h)
			w := doNote(t, s, bytes.NewReader(b), ct, h)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want 401", w.Code)
			}
			if f := vaultFiles(t, s.vault); len(f) != 0 {
				t.Fatalf("a rejected request wrote %v", f)
			}
		})
	}
}

// A good signature does not stand in for the bearer.
func TestSignatureDoesNotReplaceBearer(t *testing.T) {
	s := signedServer(t)
	body, ct := noteBody(t)
	h := signHeaders(testHMACSecret, sigNow.Unix(), "d-4", "single-click-hold", false, body)
	req := httptest.NewRequest("POST", "/note", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestParseHMACSecret(t *testing.T) {
	if _, err := parseHMACSecret(""); err == nil {
		t.Error("empty secret accepted")
	}
	if _, err := parseHMACSecret(strings.Repeat("a", minHMACSecret-1)); err == nil {
		t.Error("short secret accepted")
	}
	got, err := parseHMACSecret(testHMACSecret)
	if err != nil || string(got) != testHMACSecret {
		t.Errorf("got %q, %v; want the secret used as UTF-8 text, not hex-decoded", got, err)
	}
}

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"strconv"
	"time"
)

// signatureWindow is how far the ring's clock may sit from ours, either way.
const signatureWindow = 300 * time.Second

// minHMACSecret is the shortest signing secret startup accepts.
const minHMACSecret = 32

// parseHMACSecret reads the signing secret. The Pebble app keys the HMAC with
// the UTF-8 bytes of the secret exactly as typed, so it is used as text and
// never hex-decoded (SPEC 6.2.2).
func parseHMACSecret(env string) ([]byte, error) {
	if env == "" {
		return nil, errors.New("INDEX_HMAC_SECRET is not set")
	}
	if len(env) < minHMACSecret {
		return nil, fmt.Errorf("INDEX_HMAC_SECRET is %d bytes, minimum %d", len(env), minHMACSecret)
	}
	return []byte(env), nil
}

// sigCheck is a request's signature, validated up to everything that does not
// need the body. verify finishes the job once the body has been read.
type sigCheck struct {
	mac hash.Hash
	got []byte
}

// startSignature checks the signature headers before any of the body is read,
// so a request that cannot possibly verify is bounced immediately. It returns
// nil, nil when signing is not configured (tests only; startup refuses that).
func (s *server) startSignature(r *http.Request) (*sigCheck, error) {
	if len(s.hmacSecret) == 0 {
		return nil, nil
	}
	h := r.Header
	if v := h.Get("X-Index-Webhook-Version"); v != "1" {
		return nil, fmt.Errorf("webhook version %q", truncLog(v))
	}
	sig := h.Get("X-Index-Signature")
	ts := h.Get("X-Index-Timestamp")
	delivery := h.Get("X-Index-Delivery")
	if sig == "" || ts == "" || delivery == "" {
		return nil, errors.New("missing signature header")
	}
	// Lowercase hex only, as the app sends it. Anything else cannot match.
	got := make([]byte, 0, sha256.Size)
	if len(sig) != 2*sha256.Size {
		return nil, errors.New("malformed signature")
	}
	for i := 0; i < len(sig); i += 2 {
		hi, ok1 := hexNibble(sig[i])
		lo, ok2 := hexNibble(sig[i+1])
		if !ok1 || !ok2 {
			return nil, errors.New("malformed signature")
		}
		got = append(got, hi<<4|lo)
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, errors.New("malformed timestamp")
	}
	if d := timeNow().Sub(time.Unix(secs, 0)); d > signatureWindow || d < -signatureWindow {
		return nil, errors.New("timestamp outside window")
	}
	test := "0"
	if h.Get("X-Index-Test") == "true" {
		test = "1"
	}
	mac := hmac.New(sha256.New, s.hmacSecret)
	fmt.Fprintf(mac, "v1\n%s\n%s\n%s\n%s\n", ts, delivery, h.Get("X-Index-Trigger"), test)
	return &sigCheck{mac: mac, got: got}, nil
}

// verify reports whether the HMAC of everything fed to c.mac matches.
func (c *sigCheck) verify() bool {
	return hmac.Equal(c.mac.Sum(nil), c.got)
}

func hexNibble(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	}
	return 0, false
}

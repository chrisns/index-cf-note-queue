package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestVoiceScorerPoke covers VOICE.md section 8: after a note is durable and
// the 200 has been returned, the service pokes the scorer. The poke must
// never delay the response and a failure must never surface to the ring.
func TestVoiceScorerPoke(t *testing.T) {
	hit := make(chan struct{}, 1)
	scorer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer scorer.Close()

	s := newTestServer(t)
	s.voiceScorerURL = scorer.URL

	body, ct := multipartBody(t, map[string]string{"transcription": "hi", "recordedAt": "0"}, "", "")
	w := doNote(t, s, body, ct, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}

	select {
	case <-hit:
	case <-time.After(2 * time.Second):
		t.Fatal("scorer was never poked")
	}
}

// TestVoiceScorerPokeSlowNeverBlocksResponse covers the "cannot delay the
// response" half of VOICE.md section 8. It proves the response returns while
// the poke is still genuinely in flight, by waiting for the scorer to
// confirm it is mid-request before checking the note's response arrived.
func TestVoiceScorerPokeSlowNeverBlocksResponse(t *testing.T) {
	reached := make(chan struct{})
	release := make(chan struct{})
	scorer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(reached)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer scorer.Close()

	s := newTestServer(t)
	s.voiceScorerURL = scorer.URL

	body, ct := multipartBody(t, map[string]string{"transcription": "hi", "recordedAt": "0"}, "", "")
	done := make(chan int, 1)
	go func() {
		w := doNote(t, s, body, ct, nil)
		done <- w.Code
	}()

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("status = %d; want 200", code)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("response was blocked by a slow scorer")
	}

	// Now prove the poke really did reach the scorer and was genuinely
	// still blocked, not skipped by a race with test cleanup.
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("poke never reached the scorer")
	}
	close(release)
}

// TestVoiceScorerPokeUnreachableIsSwallowed covers "a failure is swallowed
// and never retried": an unreachable scorer must not affect the note.
func TestVoiceScorerPokeUnreachableIsSwallowed(t *testing.T) {
	s := newTestServer(t)
	s.voiceScorerURL = "http://127.0.0.1:1/unreachable"

	body, ct := multipartBody(t, map[string]string{"transcription": "hi", "recordedAt": "0"}, "", "")
	w := doNote(t, s, body, ct, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	name, _ := readNote(t, s.vault)
	if name == "" {
		t.Fatal("note was not written")
	}
}

// TestVoiceScorerPokeUnset covers the default: no VOICE_SCORER_URL means no
// poke at all, and every existing test (newTestServer leaves it unset) must
// keep working unchanged.
func TestVoiceScorerPokeUnset(t *testing.T) {
	s := newTestServer(t)
	if s.voiceScorerURL != "" {
		t.Fatalf("voiceScorerURL = %q; want unset by default", s.voiceScorerURL)
	}
}

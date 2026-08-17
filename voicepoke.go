package main

import (
	"context"
	"log"
	"net/http"
	"time"
)

// voiceScorerClient bounds the poke so a dead scorer can never hold a
// goroutine, or the process, open indefinitely.
var voiceScorerClient = &http.Client{Timeout: 5 * time.Second}

// pokeVoiceScorer implements VOICE.md section 8: after the note is durable
// and the 200 has been returned, poke the scorer. It carries no note data,
// cannot delay or endanger the response, and a failure is swallowed and
// never retried. Call only after w.WriteHeader(http.StatusOK).
func (s *server) pokeVoiceScorer() {
	if s.voiceScorerURL == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.voiceScorerURL, nil)
		if err != nil {
			// A malformed VOICE_SCORER_URL would otherwise fail silently
			// forever, with no signal anywhere that the poke never fires.
			log.Printf("voice scorer poke: bad VOICE_SCORER_URL: %v", err)
			return
		}
		resp, err := voiceScorerClient.Do(req)
		if err != nil {
			// Never retried, per VOICE.md 8: the hourly sweep is what makes
			// this safe to lose.
			log.Printf("voice scorer poke failed: %v", err)
			return
		}
		resp.Body.Close()
	}()
}

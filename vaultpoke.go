package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"
)

// Separate client from the voice scorer's: a slow or wedged vault sync must not
// consume the connection budget of an unrelated advisory feature.
var vaultPokeClient = &http.Client{Timeout: 5 * time.Second}

// pokeVaultSync tells the Obsidian vault sync that a note has landed, so it can
// pull it immediately instead of waiting out its poll interval.
//
// Same contract as pokeVoiceScorer, and for the same reason (VOICE.md 8): it
// carries no note data, it runs after the 200 has been written, it cannot delay
// or endanger the response, and a failure is swallowed and never retried. The
// sync's own periodic pass is what makes the poke safe to lose. Neither path is
// authoritative and both do the same idempotent pass.
func (s *server) pokeVaultSync() {
	if s.vaultPokeURL == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.vaultPokeURL, nil)
		if err != nil {
			// A malformed URL would otherwise fail silently forever, with no
			// signal anywhere that the poke never fires.
			log.Printf("vault sync poke: bad VAULT_POKE_URL: %v", err)
			return
		}
		// The endpoint sits on the vault's MCP service, which authenticates
		// every request. Unset means the endpoint is open, which is a
		// deployment choice rather than something to enforce here.
		if s.vaultPokeToken != "" {
			req.Header.Set("Authorization", "Bearer "+s.vaultPokeToken)
		}
		resp, err := vaultPokeClient.Do(req)
		if err != nil {
			log.Printf("vault sync poke failed: %v", err)
			return
		}
		resp.Body.Close()
		// A 401 here is silent otherwise, and looks identical to a sync that is
		// merely slow: notes would arrive only on the poll, with no clue why.
		if resp.StatusCode >= 400 {
			log.Printf("vault sync poke rejected: %s", resp.Status)
		}
	}()
}

func vaultPokeFromEnv() (url, token string) {
	return os.Getenv("VAULT_POKE_URL"), os.Getenv("VAULT_POKE_TOKEN")
}

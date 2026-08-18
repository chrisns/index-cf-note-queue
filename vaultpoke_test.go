package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// The poke must not be able to break note ingest. These assert the three
// properties VOICE.md 8 requires of it: it fires, it carries no data, and every
// failure mode is swallowed.
func TestVaultPokeFiresWithToken(t *testing.T) {
	var mu sync.Mutex
	var gotAuth, gotMethod string
	var gotLen int64
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth, gotMethod, gotLen = r.Header.Get("Authorization"), r.Method, r.ContentLength
		mu.Unlock()
		close(done)
	}))
	defer srv.Close()

	s := &server{vaultPokeURL: srv.URL, vaultPokeToken: "sekrit"}
	s.pokeVaultSync()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("poke never arrived")
	}
	mu.Lock()
	defer mu.Unlock()
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q; want POST", gotMethod)
	}
	if gotAuth != "Bearer sekrit" {
		t.Errorf("auth = %q; want the bearer token", gotAuth)
	}
	if gotLen > 0 {
		t.Errorf("body length = %d; the poke must carry no note data", gotLen)
	}
}

func TestVaultPokeUnsetIsNoop(t *testing.T) {
	s := &server{}
	s.pokeVaultSync() // must not panic and must not dial anything
}

// A dead endpoint, a 401, and a malformed URL must all be survivable: ingest has
// already returned 200 and the periodic sync will pick the note up regardless.
func TestVaultPokeSwallowsFailures(t *testing.T) {
	for name, url := range map[string]string{
		"unreachable": "http://127.0.0.1:1/nope",
		"malformed":   "http://%zz",
	} {
		t.Run(name, func(t *testing.T) {
			s := &server{vaultPokeURL: url}
			s.pokeVaultSync()
			time.Sleep(200 * time.Millisecond)
		})
	}
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer reject.Close()
	s := &server{vaultPokeURL: reject.URL}
	s.pokeVaultSync()
	time.Sleep(200 * time.Millisecond)
}

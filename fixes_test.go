package main

// Pins for the three behaviour fixes from the code review: no ServeMux
// canonicalisation redirect, never-reject on an unparseable Content-Type,
// and synthesised-id audio collisions regenerating instead of dropping.

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SPEC 6.1: there is no redirect anywhere. ServeMux would 301 "//note" to
// "/note"; the path guard must 404 it before the mux can.
func TestNonCanonicalPathIs404(t *testing.T) {
	s := newTestServer(t)
	for _, path := range []string{"//note", "/note/../note", "/note/"} {
		req := httptest.NewRequest("POST", path, strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer "+testToken1)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		if w.Code != 404 {
			t.Errorf("POST %q = %d, want 404 (a 3xx would turn the ring's POST into a GET)", path, w.Code)
		}
	}
}

// SPEC 6.5: an authenticated body we cannot parse at all is still not a
// reason to reject. It becomes the empty-request note, marked truncated.
func TestBadContentTypeWritesEvidenceNote(t *testing.T) {
	fixClock(t, time.Date(2026, 8, 16, 14, 32, 7, 0, london))
	s := newTestServer(t)
	w := doNote(t, s, strings.NewReader("not multipart at all"), "text/plain", nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	_, content := readNote(t, s.vault)
	if !strings.Contains(content, "truncated: true") {
		t.Fatalf("note must record the unreadable body via truncated: true; got:\n%s", content)
	}
}

// SPEC 7.4: EEXIST on a synthesised id is a different recording, so the id
// regenerates and both audio files survive, each note embedding its own.
func TestSynthesisedAudioCollisionRegenerates(t *testing.T) {
	fixClock(t, time.Date(2026, 8, 16, 14, 32, 7, 0, london))
	s := newTestServer(t)

	// Deterministic randHex: same value twice (id collision on the second
	// request), then a fresh value for the regeneration.
	vals := []string{"aaaa", "aaaa", "bbbb"}
	old := randHex
	randHex = func(int) string {
		v := vals[0]
		if len(vals) > 1 {
			vals = vals[1:]
		}
		return v
	}
	t.Cleanup(func() { randHex = old })

	for i, transcript := range []string{"first recording", "second recording"} {
		// Hostile audio filename "???" sanitises to nothing, forcing synthesis.
		body, ct := multipartBody(t, map[string]string{
			"transcription": transcript,
			"recordedAt":    "1786973527000",
		}, "???.m4a", "AUDIO"+transcript)
		if w := doNote(t, s, body, ct, nil); w.Code != 200 {
			t.Fatalf("request %d status = %d, want 200", i, w.Code)
		}
	}

	audio, err := filepath.Glob(filepath.Join(s.vault, attachmentsDir, "*.m4a"))
	if err != nil || len(audio) != 2 {
		t.Fatalf("want 2 distinct audio files, got %v (err %v)", audio, err)
	}
	// Each note's embed must reference a file that exists with its content.
	notes, _ := filepath.Glob(filepath.Join(s.vault, "2026", "*.md"))
	if len(notes) != 2 {
		t.Fatalf("want 2 notes, got %v", notes)
	}
	for _, p := range notes {
		b, _ := os.ReadFile(p)
		i := strings.Index(string(b), "![[")
		j := strings.Index(string(b), ".m4a]]")
		if i < 0 || j < 0 {
			t.Fatalf("note %s has no embed:\n%s", p, b)
		}
		id := string(b)[i+3 : j]
		if _, err := os.Stat(filepath.Join(s.vault, attachmentsDir, id+".m4a")); err != nil {
			t.Errorf("note %s embeds %s.m4a which does not exist", p, id)
		}
	}
}

// SPEC 6.7: the vault directory arrives root-owned 0777 from the NFS
// provisioner, so an unprivileged pod cannot chmod it. That must not stop
// startup; the probe write is the real gate.
func TestPrepareVaultSurvivesUndeniableChmod(t *testing.T) {
	fixClock(t, time.Date(2026, 8, 17, 6, 0, 0, 0, london))
	old := chmodDir
	chmodDir = func(string, os.FileMode) error { return os.ErrPermission }
	t.Cleanup(func() { chmodDir = old })

	vault := t.TempDir()
	if err := prepareVault(vault); err != nil {
		t.Fatalf("prepareVault must survive a denied chmod, got: %v", err)
	}
	for _, sub := range []string{"2026", attachmentsDir} {
		if _, err := os.Stat(filepath.Join(vault, sub)); err != nil {
			t.Errorf("expected %s to be created: %v", sub, err)
		}
	}
}

// A vault that cannot be written is still fatal: that is the real gate.
// Mirrors production, where the chmod is denied as well, so it cannot quietly
// make the directory writable again.
func TestPrepareVaultFailsWhenUnwritable(t *testing.T) {
	old := chmodDir
	chmodDir = func(string, os.FileMode) error { return os.ErrPermission }
	t.Cleanup(func() { chmodDir = old })

	vault := t.TempDir()
	if err := os.Chmod(vault, 0o500); err != nil {
		t.Skip("cannot make a read-only dir here")
	}
	t.Cleanup(func() { os.Chmod(vault, 0o755) })
	if err := prepareVault(vault); err == nil {
		t.Fatal("prepareVault must fail when the vault cannot be written")
	}
}

package main

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testToken1 = "0123456789abcdef0123456789abcdef"
	testToken2 = "fedcba9876543210fedcba9876543210"
)

func newTestServer(t *testing.T) *server {
	t.Helper()
	digests, err := parseTokens(testToken1 + "," + testToken2)
	if err != nil {
		t.Fatal(err)
	}
	return &server{vault: t.TempDir(), tokenDigests: digests}
}

func doNote(t *testing.T, s *server, body io.Reader, contentType string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/note", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+testToken1)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	return w
}

// readNote returns the single markdown file in the vault's year directory.
func readNote(t *testing.T, vault string) (string, string) {
	t.Helper()
	var notes []string
	for _, f := range vaultFiles(t, vault) {
		if strings.HasSuffix(f, ".md") {
			notes = append(notes, f)
		}
	}
	if len(notes) != 1 {
		t.Fatalf("got %v; want exactly one note", notes)
	}
	b, err := os.ReadFile(filepath.Join(vault, notes[0]))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Base(notes[0]), string(b)
}

func multipartBody(t *testing.T, parts map[string]string, audioName, audio string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if audioName != "" {
		fw, err := mw.CreateFormFile("audio", audioName)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(fw, audio)
	}
	for name, val := range parts {
		if err := mw.WriteField(name, val); err != nil {
			t.Fatal(err)
		}
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

const fixtureContentType = "multipart/form-data; boundary=9f6b7cbe-0f39-4d8a-a1f4-3f0c8a2b9d11"

func fixture(t *testing.T, name string) io.Reader {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

func TestAuth(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct {
		name   string
		header string
		want   int
	}{
		{"valid token", "Bearer " + testToken1, http.StatusOK},
		{"second token in the list", "Bearer " + testToken2, http.StatusOK},
		{"wrong token", "Bearer " + strings.Repeat("x", 32), http.StatusUnauthorized},
		{"empty header", "", http.StatusUnauthorized},
		{"bearer with empty token", "Bearer ", http.StatusUnauthorized},
		{"not bearer", testToken1, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, ct := multipartBody(t, map[string]string{"transcription": "hi", "recordedAt": "0"}, "", "")
			req := httptest.NewRequest("POST", "/note", body)
			req.Header.Set("Content-Type", ct)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Errorf("status = %d; want %d", w.Code, tc.want)
			}
		})
	}
}

func TestRouting(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/healthz", http.StatusOK},
		{"POST", "/healthz", http.StatusMethodNotAllowed},
		{"GET", "/note", http.StatusMethodNotAllowed},
		{"GET", "/", http.StatusNotFound},
		{"GET", "/anything", http.StatusNotFound},
		{"POST", "/note/", http.StatusNotFound},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("%s %s = %d; want %d", tc.method, tc.path, w.Code, tc.want)
		}
		if loc := w.Header().Get("Location"); loc != "" {
			t.Errorf("%s %s redirected to %q; redirects are forbidden", tc.method, tc.path, loc)
		}
	}
}

// TestFixture posts the hand-built fixtures from SPEC section 5 in the exact
// order the ring uses, and in a different order, and expects identical results.
func TestFixture(t *testing.T) {
	for _, name := range []string{"ring-order.bin", "reordered.bin"} {
		t.Run(name, func(t *testing.T) {
			fixClock(t, fixedTime)
			s := newTestServer(t)
			w := doNote(t, s, fixture(t, name), fixtureContentType, map[string]string{
				"X-Index-Trigger": "double-click-hold",
				"X-Audio-Size":    "19", // len("FAKEAAC-AUDIO-BYTES")
			})
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body %q", w.Code, w.Body)
			}
			audio, err := os.ReadFile(filepath.Join(s.vault, "attachments", "01J8XQ4T7V.m4a"))
			if err != nil {
				t.Fatal(err)
			}
			if string(audio) != "FAKEAAC-AUDIO-BYTES" {
				t.Errorf("audio content = %q", audio)
			}
			name, content := readNote(t, s.vault)
			if want := "2026-08-16 1432 remember-to-order-more-filament.md"; name != want {
				t.Errorf("note filename = %q; want %q", name, want)
			}
			golden, err := os.ReadFile(filepath.Join("testdata", "golden", "both.md"))
			if err != nil {
				t.Fatal(err)
			}
			if content != string(golden) {
				t.Errorf("note content:\n%s\nwant:\n%s", content, golden)
			}
		})
	}
}

// TestPayloadCombinations drives the four rows of SPEC 6.5 through the handler.
func TestPayloadCombinations(t *testing.T) {
	const recordedAtMs = "1786887127000" // 2026-08-16T14:32:07+01:00
	for _, tc := range []struct {
		name           string
		parts          map[string]string
		audio          bool
		wantEmbed      bool
		wantText       bool
		wantAttachment bool
	}{
		{"both", map[string]string{"transcription": "hello", "recordedAt": recordedAtMs, "client": "ring"}, true, true, true, true},
		{"transcription only", map[string]string{"transcription": "hello", "recordedAt": recordedAtMs}, false, false, true, false},
		{"audio only", map[string]string{"recordedAt": recordedAtMs}, true, true, false, true},
		{"neither", map[string]string{"recordedAt": recordedAtMs}, false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, fixedTime)
			s := newTestServer(t)
			audioName, audioData := "", ""
			if tc.audio {
				audioName, audioData = "01J8XQ4T7V.m4a", "FAKEAAC-AUDIO-BYTES"
			}
			body, ct := multipartBody(t, tc.parts, audioName, audioData)
			if w := doNote(t, s, body, ct, nil); w.Code != http.StatusOK {
				t.Fatalf("status = %d, body %q", w.Code, w.Body)
			}
			_, content := readNote(t, s.vault)
			if got := strings.Contains(content, "![[01J8XQ4T7V.m4a]]"); got != tc.wantEmbed {
				t.Errorf("embed present = %v; want %v\n%s", got, tc.wantEmbed, content)
			}
			if got := strings.Contains(content, "hello"); got != tc.wantText {
				t.Errorf("text present = %v; want %v\n%s", got, tc.wantText, content)
			}
			_, err := os.Stat(filepath.Join(s.vault, "attachments", "01J8XQ4T7V.m4a"))
			if got := err == nil; got != tc.wantAttachment {
				t.Errorf("attachment present = %v; want %v", got, tc.wantAttachment)
			}
			if got := strings.Contains(content, "recordingId: 01J8XQ4T7V"); got != tc.audio {
				t.Errorf("recordingId present = %v; want %v\n%s", got, tc.audio, content)
			}
			if strings.Contains(content, "clockSource") {
				t.Errorf("unexpected clockSource:\n%s", content)
			}
		})
	}
}

// TestTruncatedBody posts a fixture whose body stops mid-part: the completed
// audio is kept, the half-delivered transcription is dropped, and the note is
// marked truncated with a 200 (SPEC 6.4).
func TestTruncatedBody(t *testing.T) {
	fixClock(t, fixedTime)
	s := newTestServer(t)
	w := doNote(t, s, fixture(t, "truncated.bin"), fixtureContentType, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if _, err := os.Stat(filepath.Join(s.vault, "attachments", "01J8XQ4T7V.m4a")); err != nil {
		t.Errorf("completed audio part not written: %v", err)
	}
	_, content := readNote(t, s.vault)
	if !strings.Contains(content, "truncated: true") {
		t.Errorf("missing truncated: true:\n%s", content)
	}
	if strings.Contains(content, "Remember") {
		t.Errorf("half-delivered transcription must not be written:\n%s", content)
	}
	// No recordedAt part completed, so the server clock is used and flagged.
	if !strings.Contains(content, "clockSource: server") {
		t.Errorf("missing clockSource: server:\n%s", content)
	}
}

// TestClockFallback: a zero recordedAt must fall back to the server clock,
// visibly (SPEC 7.5).
func TestClockFallback(t *testing.T) {
	fixClock(t, fixedTime)
	s := newTestServer(t)
	body, ct := multipartBody(t, map[string]string{"transcription": "late note", "recordedAt": "0"}, "", "")
	if w := doNote(t, s, body, ct, nil); w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	name, content := readNote(t, s.vault)
	if want := "2026-08-16 1432 late-note.md"; name != want {
		t.Errorf("filename = %q; want %q", name, want)
	}
	if !strings.Contains(content, "clockSource: server") {
		t.Errorf("missing clockSource: server:\n%s", content)
	}
	if !strings.Contains(content, "recordedAt: 2026-08-16T14:32:07+01:00") {
		t.Errorf("recordedAt not the server clock:\n%s", content)
	}
}

// TestOversizedBody: over the cap before any part completes is the only 413.
func TestOversizedBody(t *testing.T) {
	s := newTestServer(t)
	body, ct := multipartBody(t, nil, "big.m4a", strings.Repeat("a", maxBody+1))
	if w := doNote(t, s, body, ct, nil); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d; want 413", w.Code)
	}
	if files := vaultFiles(t, s.vault); len(files) != 0 {
		t.Errorf("nothing should be written, got %v", files)
	}
}

// TestOversizedAfterComplete: hitting the cap after a part completed writes
// what arrived and returns 200 with truncated: true (SPEC 6.4).
func TestOversizedAfterComplete(t *testing.T) {
	fixClock(t, fixedTime)
	s := newTestServer(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("transcription", "made it in")
	fw, _ := mw.CreateFormFile("audio", "big.m4a")
	io.WriteString(fw, strings.Repeat("a", maxBody))
	mw.Close()
	if w := doNote(t, s, &buf, mw.FormDataContentType(), nil); w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	_, content := readNote(t, s.vault)
	if !strings.Contains(content, "truncated: true") || !strings.Contains(content, "made it in") {
		t.Errorf("want truncated note with the completed transcription:\n%s", content)
	}
}

// TestAudioSizeMismatch: X-Audio-Size disagreeing with the received byte count
// is flagged, never rejected (SPEC 7.6).
func TestAudioSizeMismatch(t *testing.T) {
	fixClock(t, fixedTime)
	s := newTestServer(t)
	body, ct := multipartBody(t, nil, "01J8XQ4T7V.m4a", "FAKEAAC-AUDIO-BYTES")
	if w := doNote(t, s, body, ct, map[string]string{"X-Audio-Size": "999"}); w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	_, content := readNote(t, s.vault)
	if !strings.Contains(content, "audioSizeMismatch: true") {
		t.Errorf("missing audioSizeMismatch: true:\n%s", content)
	}
}

// TestSynthesisedID: an unusable audio filename gets a synthesised id (SPEC 7.2).
func TestSynthesisedID(t *testing.T) {
	fixClock(t, fixedTime)
	fixRand(t, "c0de")
	s := newTestServer(t)
	body, ct := multipartBody(t, map[string]string{"recordedAt": "1786887127000"}, "../../etc/....m4a", "FAKEAAC")
	if w := doNote(t, s, body, ct, nil); w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if _, err := os.Stat(filepath.Join(s.vault, "attachments", "20260816T143207-c0de.m4a")); err != nil {
		t.Errorf("synthesised attachment: %v", err)
	}
	_, content := readNote(t, s.vault)
	if !strings.Contains(content, "recordingIdSynthesised: true") || !strings.Contains(content, "recordingId: 20260816T143207-c0de") {
		t.Errorf("want synthesised id in frontmatter:\n%s", content)
	}
}

// TestBogusTrigger: a value outside the allowlist must never become a fake one.
func TestBogusTrigger(t *testing.T) {
	fixClock(t, fixedTime)
	s := newTestServer(t)
	body, ct := multipartBody(t, map[string]string{"transcription": "hi", "recordedAt": "1786887127000"}, "", "")
	if w := doNote(t, s, body, ct, map[string]string{"X-Index-Trigger": "triple-click"}); w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if _, content := readNote(t, s.vault); strings.Contains(content, "trigger") {
		t.Errorf("bogus trigger leaked into frontmatter:\n%s", content)
	}
}

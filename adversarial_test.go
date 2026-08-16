package main

// Adversarial review tests. The invariant under test throughout: status 200
// if and only if the vault holds exactly one markdown note for the request,
// and no temp files leak on any path (SPEC 6.4, 7.8).

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// vaultAllFiles is vaultFiles without the .tmp- blind spot: temp files count.
func vaultAllFiles(t *testing.T, dir string) (notes, tmps, other []string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		switch {
		case strings.HasPrefix(d.Name(), ".tmp-"):
			tmps = append(tmps, rel)
		case strings.HasSuffix(d.Name(), ".md"):
			notes = append(notes, rel)
		default:
			other = append(other, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

// TestNastyBodiesInvariant throws attacker-shaped bodies at the real handler.
// It must never panic, must return 200 iff a note was written, and must never
// leave a temp file behind.
func TestNastyBodiesInvariant(t *testing.T) {
	const bd = "BOUNDARY"
	ct := "multipart/form-data; boundary=" + bd
	mp := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	cases := []struct {
		name string
		ct   string
		body string
	}{
		{"garbage not multipart at all", ct, "this is not multipart"},
		{"no boundary param", "multipart/form-data", "whatever"},
		{"empty body", ct, ""},
		{"part with no content-disposition", ct, mp("--BOUNDARY\nX-Weird: yes\n\ndata\n--BOUNDARY--\n")},
		{"content-disposition with no name", ct, mp("--BOUNDARY\nContent-Disposition: form-data\n\ndata\n--BOUNDARY--\n")},
		{"audio part with no filename", ct, mp("--BOUNDARY\nContent-Disposition: form-data; name=\"audio\"\n\nAUDIOBYTES\n--BOUNDARY--\n")},
		{"rfc2231 encoded traversal filename", ct, mp("--BOUNDARY\nContent-Disposition: form-data; name=\"audio\"; filename*=UTF-8''%2e%2e%2f%2e%2e%2fetc%2fpasswd\n\nAUDIOBYTES\n--BOUNDARY--\n")},
		{"filename with quotes and backslashes", ct, mp("--BOUNDARY\nContent-Disposition: form-data; name=\"audio\"; filename=\"a\\\"b/../c.m4a\"\n\nAUDIOBYTES\n--BOUNDARY--\n")},
		{"recordedAt overflow", ct, mp("--BOUNDARY\nContent-Disposition: form-data; name=\"recordedAt\"\n\n99999999999999999999999999\n--BOUNDARY--\n")},
		{"recordedAt max int64", ct, mp("--BOUNDARY\nContent-Disposition: form-data; name=\"recordedAt\"\n\n9223372036854775807\n--BOUNDARY--\n")},
		{"recordedAt min int64", ct, mp("--BOUNDARY\nContent-Disposition: form-data; name=\"recordedAt\"\n\n-9223372036854775808\n--BOUNDARY--\n")},
		{"recordedAt NUL bytes", ct, mp("--BOUNDARY\nContent-Disposition: form-data; name=\"recordedAt\"\n\n17868\x0087127000\n--BOUNDARY--\n")},
		{"truncated mid part headers", ct, mp("--BOUNDARY\nContent-Disposition: form-da")},
		{"boundary never appears", ct, "preamble only, no boundary line ever\n"},
		{"final boundary missing", ct, mp("--BOUNDARY\nContent-Disposition: form-data; name=\"transcription\"\n\nhalf a thought")},
		{"duplicate audio parts", ct, mp("--BOUNDARY\nContent-Disposition: form-data; name=\"audio\"; filename=\"a.m4a\"\n\nFIRST\n--BOUNDARY\nContent-Disposition: form-data; name=\"audio\"; filename=\"b.m4a\"\n\nSECOND\n--BOUNDARY--\n")},
		{"many empty parts", ct, mp(strings.Repeat("--BOUNDARY\nContent-Disposition: form-data; name=\"x\"\n\n\n", 500) + "--BOUNDARY--\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, fixedTime)
			s := newTestServer(t)
			w := doNote(t, s, strings.NewReader(tc.body), tc.ct, nil)
			notes, tmps, _ := vaultAllFiles(t, s.vault)
			if len(tmps) != 0 {
				t.Errorf("leaked temp files: %v", tmps)
			}
			switch w.Code {
			case http.StatusOK:
				if len(notes) != 1 {
					t.Errorf("200 but %d notes in vault: %v", len(notes), notes)
				}
			case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
				if len(notes) != 0 {
					t.Errorf("%d with notes written: %v", w.Code, notes)
				}
			default:
				t.Errorf("unexpected status %d", w.Code)
			}
		})
	}
}

// TestOversizedPreamble: the cap trips while multipart is still hunting for the
// first boundary, so no part completed. SPEC 6.4: that is the 413 case, and
// the MaxBytesError must survive its trip through mime/multipart's reader.
// (The preamble carries newlines: a single giant line dies at bufio's buffer
// limit long before the cap, which is a malformed body, not an oversized one.)
func TestOversizedPreamble(t *testing.T) {
	s := newTestServer(t)
	body := io.MultiReader(
		strings.NewReader(strings.Repeat("xxxxxxxxxxxxxxx\r\n", maxBody/16)),
		strings.NewReader("\r\n--BOUNDARY--\r\n"),
	)
	w := doNote(t, s, body, "multipart/form-data; boundary=BOUNDARY", nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d; want 413", w.Code)
	}
	notes, tmps, other := vaultAllFiles(t, s.vault)
	if len(notes)+len(tmps)+len(other) != 0 {
		t.Errorf("vault not empty: %v %v %v", notes, tmps, other)
	}
}

// TestOversizedMidAudioAfterTranscription: transcription completes, then the
// cap trips mid-audio. SPEC 6.4: finalize what completed, truncated: true,
// 200 — and the half-streamed audio temp file must be cleaned up.
func TestOversizedMidAudioAfterTranscription(t *testing.T) {
	fixClock(t, fixedTime)
	s := newTestServer(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("transcription", "the words made it")
	fw, _ := mw.CreateFormFile("audio", "01J8XQ4T7V.m4a")
	io.WriteString(fw, strings.Repeat("a", maxBody))
	mw.Close()
	w := doNote(t, s, &buf, mw.FormDataContentType(), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	notes, tmps, other := vaultAllFiles(t, s.vault)
	if len(tmps) != 0 {
		t.Errorf("leaked audio temp: %v", tmps)
	}
	if len(other) != 0 {
		t.Errorf("incomplete audio must not be placed: %v", other)
	}
	if len(notes) != 1 {
		t.Fatalf("want one note, got %v", notes)
	}
	b, _ := os.ReadFile(filepath.Join(s.vault, notes[0]))
	if !strings.Contains(string(b), "truncated: true") || !strings.Contains(string(b), "the words made it") {
		t.Errorf("want truncated note carrying the completed transcription:\n%s", b)
	}
	if strings.Contains(string(b), "![[") {
		t.Errorf("note must not embed audio that was never placed:\n%s", b)
	}
}

// TestConcurrentDuplicateDelivery runs the same request through two handler
// goroutines at once — the two-replica case of SPEC 7.4. Both must 200, and
// exactly one attachment and one note must exist.
func TestConcurrentDuplicateDelivery(t *testing.T) {
	fixClock(t, fixedTime)
	s := newTestServer(t)
	build := func() (io.Reader, string) {
		return multipartBody(t, map[string]string{
			"transcription": "remember to order more filament",
			"recordedAt":    "1786887127000",
		}, "01J8XQ4T7V.m4a", "FAKEAAC-AUDIO-BYTES")
	}
	var wg sync.WaitGroup
	codes := make([]int, 2)
	bodies := make([]io.Reader, 2)
	cts := make([]string, 2)
	for i := range bodies {
		bodies[i], cts[i] = build()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/note", bodies[i])
			req.Header.Set("Content-Type", cts[i])
			req.Header.Set("Authorization", "Bearer "+testToken1)
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, req)
			codes[i] = w.Code
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d: status %d; want 200", i, c)
		}
	}
	notes, tmps, other := vaultAllFiles(t, s.vault)
	if len(tmps) != 0 {
		t.Errorf("leaked temps: %v", tmps)
	}
	if len(notes) != 1 {
		t.Errorf("duplicate delivery produced %d notes: %v", len(notes), notes)
	}
	if len(other) != 1 || other[0] != filepath.Join("attachments", "01J8XQ4T7V.m4a") {
		t.Errorf("want exactly the one attachment, got %v", other)
	}
}

// TestConcurrentDistinctNotesSameMinute: two different notes, same minute,
// same slug, no audio — SPEC 7.4's missing-never-equals-missing under
// concurrency. Both must survive as separate files.
func TestConcurrentDistinctNotesSameMinute(t *testing.T) {
	fixClock(t, fixedTime)
	s := newTestServer(t)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		body, ct := multipartBody(t, map[string]string{
			"transcription": "same words",
			"recordedAt":    "1786887127000",
		}, "", "")
		wg.Add(1)
		go func(body io.Reader, ct string) {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/note", body)
			req.Header.Set("Content-Type", ct)
			req.Header.Set("Authorization", "Bearer "+testToken1)
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, req)
			codes[i] = w.Code
		}(body, ct)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d: status %d; want 200", i, c)
		}
	}
	notes, _, _ := vaultAllFiles(t, s.vault)
	if len(notes) != 2 {
		t.Errorf("two distinct audio-less notes collapsed: got %v; want 2 files", notes)
	}
}

// TestNFKDNormalisation: SPEC 7.2 step 1 and 7.3 say NFKD-normalise before
// filtering, so accented speech keeps its base letters. The implementation
// skips NFKD (see the ponytail comment in note.go), dropping the letters
// instead.
func TestNFKDNormalisation(t *testing.T) {
	if got, ok := sanitiseRecordingID("névé.m4a"); !ok || got != "neve" {
		t.Errorf("sanitiseRecordingID(névé.m4a) = %q, %v; SPEC 7.2 NFKD wants %q", got, ok, "neve")
	}
	if got := slug("café latte"); got != "cafe-latte" {
		t.Errorf("slug(café latte) = %q; SPEC 7.3 NFKD wants %q", got, "cafe-latte")
	}
}

// TestAudioPartHeadersOnly: a part that names the audio field but dies before
// any body byte. The temp file exists with zero bytes and audioComplete is
// false — nothing must be placed and nothing must leak.
func TestAudioPartHeadersOnly(t *testing.T) {
	fixClock(t, fixedTime)
	s := newTestServer(t)
	raw := "--B\r\nContent-Disposition: form-data; name=\"audio\"; filename=\"01J8XQ4T7V.m4a\"\r\n\r\nFAKE"
	w := doNote(t, s, strings.NewReader(raw), "multipart/form-data; boundary=B", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (truncated body is never a rejection)", w.Code)
	}
	notes, tmps, other := vaultAllFiles(t, s.vault)
	if len(tmps) != 0 {
		t.Errorf("leaked temps: %v", tmps)
	}
	if len(other) != 0 {
		t.Errorf("incomplete audio placed: %v", other)
	}
	if len(notes) != 1 {
		t.Fatalf("want the truncated note, got %v", notes)
	}
	b, _ := os.ReadFile(filepath.Join(s.vault, notes[0]))
	if !strings.Contains(string(b), "truncated: true") {
		t.Errorf("missing truncated: true:\n%s", b)
	}
}

// TestQuotedPrintableFilename: a Content-Disposition using percent/2047-style
// oddities must never panic and never traverse.
func TestQuotedPrintableFilename(t *testing.T) {
	fixClock(t, fixedTime)
	s := newTestServer(t)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="audio"; filename="=?UTF-8?B?Li4vLi4vZXRjL3Bhc3N3ZA==?=.m4a"`)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	pw, err := mw.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(pw, "FAKEAAC")
	mw.Close()
	w := doNote(t, s, &buf, mw.FormDataContentType(), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	_, _, other := vaultAllFiles(t, s.vault)
	for _, f := range other {
		if !strings.HasPrefix(f, "attachments"+string(filepath.Separator)) {
			t.Errorf("file escaped attachments/: %q", f)
		}
		if strings.Contains(f, "..") {
			t.Errorf("traversal survived: %q", f)
		}
	}
}

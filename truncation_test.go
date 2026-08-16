package main

// SPEC 6.4: "a body that stops early is not a reason to reject. Write whatever
// parts completed, set truncated: true, and return 200." mime/multipart
// reports several cut-short positions as io.EOF — bare from newPart when the
// body dies mid-part-headers, and fmt-wrapped ("multipart: NextPart: %w")
// when it dies hunting the next boundary line — and handleNote's
// errors.Is(err, io.EOF) treats every one of them as a clean close delimiter.
// The note is then written WITHOUT truncated: true, so the wearer cannot tell
// the audio (or anything after the cut) was lost. Only a cut positioned
// mid-part-body surfaces as io.ErrUnexpectedEOF and gets flagged.
// The handler must distinguish a real close delimiter
// ("--boundary--") from an EOF that arrived before one.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cutShortNote posts a raw body and returns the single note's content.
func cutShortNote(t *testing.T, body string) string {
	t.Helper()
	fixClock(t, fixedTime)
	s := newTestServer(t)
	w := doNote(t, s, strings.NewReader(body), "multipart/form-data; boundary=B", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (a cut-short body is never a rejection)", w.Code)
	}
	notes, tmps, _ := vaultAllFiles(t, s.vault)
	if len(tmps) != 0 {
		t.Errorf("leaked temps: %v", tmps)
	}
	if len(notes) != 1 {
		t.Fatalf("want one note, got %v", notes)
	}
	b, err := os.ReadFile(filepath.Join(s.vault, notes[0]))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const completeTranscriptionPart = "--B\r\n" +
	"Content-Disposition: form-data; name=\"transcription\"\r\n" +
	"\r\n" +
	"the words made it\r\n"

// TestCutAfterBoundaryLineIsTruncated: the transcription part completes, the
// next boundary line arrives, then the connection dies before the audio part.
// NextPart returns bare io.EOF from the header read, indistinguishable in the
// handler from a clean close delimiter — and the note comes out claiming the
// body arrived whole, with the audio silently missing.
func TestCutAfterBoundaryLineIsTruncated(t *testing.T) {
	content := cutShortNote(t, completeTranscriptionPart+"--B\r\n")
	if !strings.Contains(content, "the words made it") {
		t.Errorf("completed transcription must be written:\n%s", content)
	}
	if !strings.Contains(content, "truncated: true") {
		t.Errorf("body stopped early after a boundary line; SPEC 6.4 wants truncated: true:\n%s", content)
	}
}

// TestCutMidSecondPartHeadersIsTruncated: same cut, one step later — the
// second part's Content-Disposition line is half-delivered. Still bare io.EOF,
// still no truncated flag.
func TestCutMidSecondPartHeadersIsTruncated(t *testing.T) {
	content := cutShortNote(t, completeTranscriptionPart+"--B\r\nContent-Disposition: form-da")
	if !strings.Contains(content, "the words made it") {
		t.Errorf("completed transcription must be written:\n%s", content)
	}
	if !strings.Contains(content, "truncated: true") {
		t.Errorf("body stopped early mid part headers; SPEC 6.4 wants truncated: true:\n%s", content)
	}
}

// TestCutInPreambleIsTruncated: the body dies before the first boundary line
// ever appears ("multipart: NextPart: EOF" — the %w-wrapped case). Nothing
// completed, so the note records an empty arrival — but it must still say the
// body was cut short.
func TestCutInPreambleIsTruncated(t *testing.T) {
	content := cutShortNote(t, "preamble, then the connection died\r\n")
	if !strings.Contains(content, "truncated: true") {
		t.Errorf("body stopped early in the preamble; SPEC 6.4 wants truncated: true:\n%s", content)
	}
}

// TestCleanCloseIsNotTruncated pins the other direction: a body ending with
// the real close delimiter must NOT be flagged. Any fix for the tests above
// that flags every EOF would break this.
func TestCleanCloseIsNotTruncated(t *testing.T) {
	content := cutShortNote(t, completeTranscriptionPart+"--B--\r\n")
	if !strings.Contains(content, "the words made it") {
		t.Errorf("transcription must be written:\n%s", content)
	}
	if strings.Contains(content, "truncated") {
		t.Errorf("clean close delimiter mislabelled as truncation:\n%s", content)
	}
}

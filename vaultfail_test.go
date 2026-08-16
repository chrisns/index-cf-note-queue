//go:build unix

package main

// A vault that fails mid-write is not a truncated body. SPEC 7.6: "truncated:
// true only when the body was cut short"; SPEC 6.4's truncation cases are the
// cap and a body that stops early — never a disk error. consumePart wraps
// CreateTemp/MkdirAll failures in errVault (500), but a write error surfaced
// by io.Copy while streaming the audio escapes unwrapped, so handleNote
// mislabels a vault failure as client truncation: the audio is silently
// discarded, the note lies with truncated: true, and the response is 200.
// Demonstrated here by capping RLIMIT_FSIZE so the audio temp file's writes
// fail with EFBIG while the small markdown still succeeds.
// The io.Copy destination-write error must be routed to
// errVault so a vault failure is a 500, not a false truncated 200.

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestVaultWriteErrorMidAudioIsNotTruncation(t *testing.T) {
	fixClock(t, fixedTime)
	s := newTestServer(t)

	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Skipf("getrlimit: %v", err)
	}
	const cap = 256 * 1024 // audio temp write fails here; the markdown is far smaller
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: cap, Max: old.Max}); err != nil {
		t.Skipf("setrlimit: %v", err)
	}
	t.Cleanup(func() { syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old) })

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("transcription", "the words made it")
	fw, _ := mw.CreateFormFile("audio", "01J8XQ4T7V.m4a")
	io.WriteString(fw, strings.Repeat("a", cap+cap/2)) // well under maxBody, over RLIMIT_FSIZE
	mw.Close()

	w := doNote(t, s, &buf, mw.FormDataContentType(), nil)

	// Either outcome is spec-legal: 500 with no markdown (vault write failed,
	// SPEC 7.8), or a 200 note that does NOT claim the body was cut short.
	// What is not legal is 200 + truncated: true for a body the client
	// delivered intact.
	notes, _, _ := vaultAllFiles(t, s.vault)
	switch w.Code {
	case http.StatusInternalServerError:
		if len(notes) != 0 {
			t.Errorf("500 but markdown present: %v (SPEC 7.8: a 500 must mean no markdown)", notes)
		}
	case http.StatusOK:
		if len(notes) != 1 {
			t.Fatalf("200 with %d notes: %v", len(notes), notes)
		}
		b, err := os.ReadFile(filepath.Join(s.vault, notes[0]))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "truncated: true") {
			t.Errorf("vault write failure mislabelled as client truncation (SPEC 7.6):\n%s", b)
		}
	default:
		t.Errorf("status = %d; want 500 (vault write failed) or an honest 200", w.Code)
	}
}

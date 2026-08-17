package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const maxBody = 16 << 20 // 16 MiB (SPEC 6.3)

const attachmentsDir = "attachments"

// vaultFail is the one 500: a vault write failed before the markdown was
// durable, so the request must not look like a success (SPEC 7.8).
func vaultFail(w http.ResponseWriter) {
	http.Error(w, "vault write failed", http.StatusInternalServerError)
}

// errVault marks a vault write failure, as opposed to a body read failure.
var errVault = errors.New("vault write failed")

// fromKnownRing reports whether the audio filename the ring sent starts with a
// configured prefix. That prefix embeds the ring's device identifier, so it is
// a second factor: a leaked bearer alone is not enough to write to the vault.
//
// A note with no audio part carries no identifier and cannot be checked, so it
// passes on the bearer alone. Rejecting it would delete a real thought, and the
// spec's rule is that a note is never rejected for something it cannot control.
func (s *server) fromKnownRing(rawFilename string) bool {
	// No prefixes configured means the check is inactive. startup() refuses to
	// run without them, so this state is unreachable in production; it exists so
	// tests can exercise sanitisation independently of this layer. Sanitisation
	// is still defence in depth: a matching prefix does not make the rest safe.
	if len(s.ringPrefixes) == 0 || rawFilename == "" {
		return true
	}
	for _, p := range s.ringPrefixes {
		if len(rawFilename) >= len(p) &&
			subtle.ConstantTimeCompare([]byte(rawFilename[:len(p)]), []byte(p)) == 1 {
			return true
		}
	}
	return false
}

func (s *server) authorised(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	// Compare digests, not raw strings: ConstantTimeCompare returns 0
	// immediately on a length mismatch, which would leak the length (SPEC 6.2).
	presented := sha256.Sum256([]byte(h[len(prefix):]))
	ok := false
	for _, want := range s.tokenDigests {
		if subtle.ConstantTimeCompare(presented[:], want[:]) == 1 {
			ok = true
		}
	}
	return ok
}

// vaultWriter marks any write error on the vault side as errVault, so a full
// disk mid-stream is never mistaken for the client's body dying (SPEC 7.8).
type vaultWriter struct {
	w io.Writer
}

func (v vaultWriter) Write(p []byte) (int, error) {
	n, err := v.w.Write(p)
	if err != nil {
		err = fmt.Errorf("%w: %v", errVault, err)
	}
	return n, err
}

// tailReader retains the last few hundred bytes read, so an io.EOF from
// mime/multipart can be checked against the real close delimiter (SPEC 6.4).
type tailReader struct {
	r    io.Reader
	tail []byte
	keep int
}

func (t *tailReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.tail = append(t.tail, p[:n]...)
		if len(t.tail) > t.keep {
			t.tail = t.tail[len(t.tail)-t.keep:]
		}
	}
	return n, err
}

// sawClose reports whether the close delimiter "--<boundary>--" passed through.
// ponytail: an epilogue longer than the retained tail would hide the delimiter
// and flag a clean body as truncated; the ring sends none.
func (t *tailReader) sawClose(boundary string) bool {
	return bytes.Contains(t.tail, []byte("--"+boundary+"--"))
}

// partState accumulates what the body actually delivered, part order unknown.
type partState struct {
	audioTmp        *os.File
	audioBytes      int64
	audioComplete   bool
	rawFilename     string
	transcription   []byte
	transcriptionOK bool
	recordedAtMs    int64
	recordedAtOK    bool
	anyComplete     bool
}

// consumePart streams one part. Audio goes straight to a temp file in the
// attachments directory; everything else is small enough for memory (SPEC 6.3).
func (s *server) consumePart(p *multipart.Part, st *partState) error {
	switch p.FormName() {
	case "audio":
		if st.audioTmp != nil {
			break // first audio part wins; drain duplicates below
		}
		st.rawFilename = p.FileName()
		dir := filepath.Join(s.vault, attachmentsDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("%w: %v", errVault, err)
		}
		f, err := os.CreateTemp(dir, ".tmp-audio-*")
		if err != nil {
			return fmt.Errorf("%w: %v", errVault, err)
		}
		st.audioTmp = f
		// A destination failure (ENOSPC, EFBIG) is a vault error, not client
		// truncation; vaultWriter tags its own side (SPEC 6.4 vs 7.8).
		n, err := io.Copy(vaultWriter{f}, p)
		st.audioBytes = n
		if err != nil {
			return err
		}
		st.audioComplete = true
		st.anyComplete = true
		return nil
	case "transcription":
		if st.transcriptionOK {
			break
		}
		b, err := io.ReadAll(p)
		if err != nil {
			return err
		}
		st.transcription = b
		st.transcriptionOK = true
		st.anyComplete = true
		return nil
	case "recordedAt":
		if st.recordedAtOK {
			break
		}
		b, err := io.ReadAll(p)
		if err != nil {
			return err
		}
		if ms, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); perr == nil {
			st.recordedAtMs = ms
			st.recordedAtOK = true
		}
		st.anyComplete = true
		return nil
	}
	// client, unknown parts, and duplicates: ignored, never a reason to reject.
	if _, err := io.Copy(io.Discard, p); err != nil {
		return err
	}
	st.anyComplete = true
	return nil
}

func isMaxBytes(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

func (s *server) handleNote(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var st partState
	defer func() {
		if st.audioTmp != nil {
			st.audioTmp.Close()
			os.Remove(st.audioTmp.Name())
		}
	}()

	truncated := false
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	boundary := params["boundary"]
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") || boundary == "" {
		// An authenticated body we cannot parse at all is still not a reason to
		// reject: no part is recoverable, so record that an unreadable request
		// arrived (SPEC 6.5). The ring would never see a 4xx again anyway.
		truncated = true
		boundary = ""
	}
	// The tail is what tells a real "--boundary--" close apart from a body that
	// died early: mime/multipart reports both as io.EOF (SPEC 6.4).
	tail := &tailReader{r: http.MaxBytesReader(w, r.Body, maxBody), keep: len(boundary) + 512}
	mr := multipart.NewReader(tail, boundary)

	for boundary != "" {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			// io.EOF also covers a body that dies after a boundary line,
			// mid-part-headers, or in the preamble. Only the close delimiter
			// makes it a clean end (SPEC 6.4).
			if !tail.sawClose(boundary) {
				truncated = true
			}
			break
		}
		if err == nil {
			err = s.consumePart(part, &st)
		}
		if err == nil {
			continue
		}
		if errors.Is(err, errVault) {
			vaultFail(w)
			return
		}
		// 413 only when the cap was hit before any part completed, so there
		// is nothing to write (SPEC 6.4). Any other mid-body failure means
		// finalize what completed, mark it truncated, and return 200.
		if isMaxBytes(err) && !st.anyComplete {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		truncated = true
		break
	}

	// Second factor, checked before anything is written. A mismatch means the
	// audio did not come from a known ring, so there is no note of yours to lose.
	if !s.fromKnownRing(st.rawFilename) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	now := timeNow().In(london)
	recorded := now
	clockServer := true
	if st.recordedAtOK && st.recordedAtMs != 0 {
		t := time.UnixMilli(st.recordedAtMs).In(london)
		// Exactly a day off is allowed; "more than a day" falls back (SPEC 7.5).
		if d := t.Sub(now); d <= 24*time.Hour && d >= -24*time.Hour {
			recorded = t
			clockServer = false
		}
	}
	recorded = recorded.Truncate(time.Second)

	n := &note{
		recordedAt:    recorded,
		clockServer:   clockServer,
		transcription: strings.TrimSpace(string(st.transcription)),
		truncated:     truncated,
	}
	switch t := r.Header.Get("X-Index-Trigger"); t {
	case "single-click-hold", "double-click-hold":
		n.trigger = t
	}
	if st.audioComplete {
		n.hasAudio = true
		if id, ok := sanitiseRecordingID(st.rawFilename); ok {
			n.recordingID = id
		} else {
			n.recordingID = synthesiseID(recorded)
			n.idSynthesised = true
		}
		if h := r.Header.Get("X-Audio-Size"); h != "" {
			if want, perr := strconv.ParseInt(strings.TrimSpace(h), 10, 64); perr == nil && want != st.audioBytes {
				n.audioSizeMismatch = true
			}
		}
	}
	// No audio part means no id and no recordingIdSynthesised flag: a
	// synthesised id exists only to name an audio file (SPEC 7.2, 7.6).

	// Audio first, then markdown: the note is the last thing to appear, so it
	// never references a file that is not there yet (SPEC 7.8).
	if n.hasAudio {
		if err := s.placeAudio(st.audioTmp, n); err != nil {
			vaultFail(w)
			return
		}
	}
	if err := s.writeMarkdown(n); err != nil {
		vaultFail(w)
		return
	}
	// Once the markdown is in place, 200 whatever happens afterwards.
	w.WriteHeader(http.StatusOK)
	s.pokeVoiceScorer()
}

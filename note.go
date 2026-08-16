package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"
)

var randHex = func(n int) string { // swapped in tests
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// note is everything the renderer needs, already validated.
type note struct {
	recordedAt        time.Time // in Europe/London, second precision
	clockServer       bool
	recordingID       string // "" when there is no audio part
	idSynthesised     bool
	hasAudio          bool
	transcription     string
	trigger           string // validated against the allowlist, or ""
	truncated         bool
	audioSizeMismatch bool
}

func isAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// sanitiseRecordingID turns the audio part's filename into a safe id (SPEC 7.2):
// NFKD-normalise, then keep only [A-Za-z0-9_-].
func sanitiseRecordingID(filename string) (string, bool) {
	var b strings.Builder
	for _, r := range norm.NFKD.String(strings.TrimSuffix(filename, ".m4a")) {
		if isAlnum(r) || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	id := b.String()
	if len(id) > 63 {
		id = id[:63]
	}
	if id == "" || id[0] == '-' || !strings.ContainsFunc(id, isAlnum) {
		return "", false
	}
	return id, true
}

// slug derives a filename fragment from the transcription (SPEC 7.3):
// NFKD-normalise, lowercase, keep [a-z0-9], collapse runs to a hyphen.
func slug(transcription string) string {
	var b strings.Builder
	pending := false // a run of non-[a-z0-9] to collapse; leading run is trimmed
	for _, r := range strings.ToLower(norm.NFKD.String(transcription)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pending && b.Len() > 0 {
				b.WriteByte('-')
			}
			pending = false
			b.WriteRune(r)
		} else {
			pending = true
		}
	}
	words := strings.Split(b.String(), "-")
	if b.Len() == 0 {
		return ""
	}
	if len(words) > 6 {
		words = words[:6]
	}
	s := strings.Join(words, "-")
	if len(s) > 60 {
		s = strings.TrimRight(s[:60], "-")
	}
	return s
}

func synthesiseID(t time.Time) string {
	return t.Format("20060102T150405") + "-" + randHex(2)
}

type frontmatter struct {
	RecordedAt             time.Time `yaml:"recordedAt"`
	RecordingID            string    `yaml:"recordingId,omitempty"`
	Source                 string    `yaml:"source"`
	Trigger                string    `yaml:"trigger,omitempty"`
	Tags                   []string  `yaml:"tags"`
	Truncated              bool      `yaml:"truncated,omitempty"`
	ClockSource            string    `yaml:"clockSource,omitempty"`
	RecordingIDSynthesised bool      `yaml:"recordingIdSynthesised,omitempty"`
	AudioSizeMismatch      bool      `yaml:"audioSizeMismatch,omitempty"`
}

// render produces the base filename (before collision suffixes) and the file
// content: frontmatter (SPEC 7.6), then embed, then transcription (SPEC 7.7).
func (n *note) render() (string, []byte) {
	fm := frontmatter{
		RecordedAt:             n.recordedAt,
		RecordingID:            n.recordingID,
		Source:                 "index-01",
		Trigger:                n.trigger,
		Tags:                   []string{"index"},
		Truncated:              n.truncated,
		RecordingIDSynthesised: n.idSynthesised,
		AudioSizeMismatch:      n.audioSizeMismatch,
	}
	if n.trigger != "" {
		fm.Tags = append(fm.Tags, "index/"+n.trigger)
	}
	if n.clockServer {
		fm.ClockSource = "server"
	}
	var buf bytes.Buffer
	buf.WriteString("---\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(fm); err != nil {
		panic(err) // static struct of strings and bools; cannot fail
	}
	enc.Close()
	buf.WriteString("---\n")
	var body []string
	if n.hasAudio {
		body = append(body, "![["+n.recordingID+".m4a]]")
	}
	if n.transcription != "" {
		body = append(body, n.transcription)
	}
	if len(body) > 0 {
		buf.WriteString("\n")
		buf.WriteString(strings.Join(body, "\n\n"))
		buf.WriteString("\n")
	}
	name := n.recordedAt.Format("2006-01-02 1504")
	if s := slug(n.transcription); s != "" {
		name += " " + s
	}
	return name + ".md", buf.Bytes()
}

// readFrontmatterID extracts recordingId from an existing note, "" when absent.
func readFrontmatterID(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(b, []byte("---\n")) {
		return ""
	}
	end := bytes.Index(b[4:], []byte("\n---\n"))
	if end < 0 {
		return ""
	}
	var fm struct {
		RecordingID string `yaml:"recordingId"`
	}
	if err := yaml.Unmarshal(b[4:4+end+1], &fm); err != nil {
		return ""
	}
	return fm.RecordingID
}

// syncDir flushes a directory after a rename/link. Best effort: on NFS the
// guarantee is weaker than local disk, and that weakness is accepted (SPEC 7.8).
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}

// writeMarkdown places the note with the collision discipline of SPEC 7.4:
// link(2) into place so EEXIST is atomic, never stat-then-rename.
func (s *server) writeMarkdown(n *note) error {
	name, content := n.render()
	dir := filepath.Join(s.vault, n.recordedAt.Format("2006"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-note-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	base := strings.TrimSuffix(name, ".md")
	try := name
	for attempt := 0; attempt < 6; attempt++ {
		err := os.Link(tmp.Name(), filepath.Join(dir, try))
		if err == nil {
			syncDir(dir)
			return nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		// Same note arriving twice: both ids present and equal keeps the
		// existing file. A missing id is never equal to a missing id.
		if existing := readFrontmatterID(filepath.Join(dir, try)); existing != "" && n.recordingID != "" && existing == n.recordingID {
			return nil
		}
		switch {
		case attempt == 0 && n.recordingID != "":
			try = fmt.Sprintf("%s-%.8s.md", base, n.recordingID)
		case attempt == 0:
			try = fmt.Sprintf("%s-%02d.md", base, n.recordedAt.Second())
		default:
			// ponytail: beyond the spec's one deterministic suffix, retry with
			// random suffixes rather than loop forever on a pathological vault.
			try = fmt.Sprintf("%s-%s.md", base, randHex(2))
		}
	}
	return errors.New("could not find a free filename")
}

// placeAudio links the streamed temp file to attachments/<id>.m4a (SPEC 7.8).
// EEXIST on a ring-supplied id is duplicate delivery of the same recording:
// keep the existing file, never overwrite an attachment this request did not
// create (SPEC 7.4). EEXIST on a synthesised id is a DIFFERENT recording that
// happened to draw the same name: regenerate and retry, and update the note so
// its embed references the file actually written.
func (s *server) placeAudio(f *os.File, n *note) error {
	defer f.Close()
	if err := f.Chmod(0o644); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	dir := filepath.Join(s.vault, attachmentsDir)
	for attempt := 0; attempt < 6; attempt++ {
		err := os.Link(f.Name(), filepath.Join(dir, n.recordingID+".m4a"))
		if err == nil || (errors.Is(err, fs.ErrExist) && !n.idSynthesised) {
			syncDir(dir)
			return nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		n.recordingID = synthesiseID(n.recordedAt)
	}
	return errors.New("could not find a free audio filename")
}

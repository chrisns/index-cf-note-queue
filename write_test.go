package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func vaultFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !strings.HasPrefix(d.Name(), ".tmp-") {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCollisionSameID(t *testing.T) {
	s := &server{vault: t.TempDir()}
	n := &note{recordedAt: fixedTime, recordingID: "01J8XQ4T7V", hasAudio: true, transcription: "same note twice"}
	if err := s.writeMarkdown(n); err != nil {
		t.Fatal(err)
	}
	if err := s.writeMarkdown(n); err != nil {
		t.Fatal(err)
	}
	files := vaultFiles(t, s.vault)
	if len(files) != 1 {
		t.Fatalf("same id twice: got %v; want one file", files)
	}
}

func TestCollisionDifferentID(t *testing.T) {
	s := &server{vault: t.TempDir()}
	a := &note{recordedAt: fixedTime, recordingID: "01J8XQ4T7V", hasAudio: true, transcription: "same words"}
	b := &note{recordedAt: fixedTime, recordingID: "01J9ZZZZZZ", hasAudio: true, transcription: "same words"}
	if err := s.writeMarkdown(a); err != nil {
		t.Fatal(err)
	}
	if err := s.writeMarkdown(b); err != nil {
		t.Fatal(err)
	}
	files := vaultFiles(t, s.vault)
	if len(files) != 2 {
		t.Fatalf("different ids: got %v; want two files", files)
	}
	suffixed := filepath.Join("2026", "2026-08-16 1432 same-words-01J9ZZZZ.md")
	if files[0] != suffixed && files[1] != suffixed {
		t.Errorf("got %v; want a file named %q", files, suffixed)
	}
}

func TestCollisionMissingIDNeverEqual(t *testing.T) {
	// Two audio-less notes in the same minute must not collapse into one,
	// even with identical content.
	s := &server{vault: t.TempDir()}
	n1 := &note{recordedAt: fixedTime, transcription: "same words"}
	n2 := &note{recordedAt: fixedTime, transcription: "same words"}
	if err := s.writeMarkdown(n1); err != nil {
		t.Fatal(err)
	}
	if err := s.writeMarkdown(n2); err != nil {
		t.Fatal(err)
	}
	files := vaultFiles(t, s.vault)
	if len(files) != 2 {
		t.Fatalf("missing ids: got %v; want two files", files)
	}
	suffixed := filepath.Join("2026", "2026-08-16 1432 same-words-07.md") // seconds from recordedAt
	if files[0] != suffixed && files[1] != suffixed {
		t.Errorf("got %v; want a file named %q", files, suffixed)
	}
}

func TestCollisionConcurrent(t *testing.T) {
	s := &server{vault: t.TempDir()}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []string{"AAAAAAAAAA", "BBBBBBBBBB"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.writeMarkdown(&note{recordedAt: fixedTime, recordingID: id, hasAudio: true, transcription: "colliding words"})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	files := vaultFiles(t, s.vault)
	if len(files) != 2 {
		t.Fatalf("concurrent writers: got %v; want two distinct files", files)
	}
	if files[0] == files[1] {
		t.Fatalf("both writers landed on %q", files[0])
	}
}

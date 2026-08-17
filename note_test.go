package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestMain(m *testing.M) {
	var err error
	london, err = time.LoadLocation("Europe/London")
	if err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

var fixedTime = time.Date(2026, 8, 16, 14, 32, 7, 0, mustLondon())

func mustLondon() *time.Location {
	l, err := time.LoadLocation("Europe/London")
	if err != nil {
		panic(err)
	}
	return l
}

func fixClock(t *testing.T, ts time.Time) {
	t.Helper()
	old := timeNow
	timeNow = func() time.Time { return ts }
	t.Cleanup(func() { timeNow = old })
}

func fixRand(t *testing.T, hexDigits string) {
	t.Helper()
	old := randHex
	randHex = func(int) string { return hexDigits }
	t.Cleanup(func() { randHex = old })
}

func TestSanitiseRecordingID(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"01J8XQ4T7V.m4a", "01J8XQ4T7V", true},
		{"../../etc/passwd", "etcpasswd", true},
		{"..", "", false},
		{".", "", false},
		{"", "", false},
		{"---.m4a", "", false},     // only hyphens: no alnum
		{"-abc.m4a", "", false},    // leading hyphen
		{"névé.m4a", "neve", true}, // NFKD keeps the base letters
		{"a_b-c.m4a", "a_b-c", true},
		{string(make([]byte, 100)), "", false}, // NULs are all dropped
	} {
		got, ok := sanitiseRecordingID(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("sanitiseRecordingID(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	long := strings.Repeat("a", 300) + ".m4a"
	if got, ok := sanitiseRecordingID(long); !ok || len(got) != maxRecordingID {
		t.Errorf("long id: got %d bytes, ok=%v; want %d, true", len(got), ok, maxRecordingID)
	}
	// Shaped like a real ring id, 82 bytes: "ring_<device uuid>-<counter>-<per
	// recording uuid>". The previous 63-byte cap truncated it mid-UUID. The
	// device UUID here is synthetic on purpose; the real one identifies a ring.
	real := "ring_00000000-1111-2222-3333-444444444444-999-aaaaaaaa-bbbb-cccc-dddd-ee.m4a"
	want := strings.TrimSuffix(real, ".m4a")
	if got, ok := sanitiseRecordingID(real); !ok || got != want {
		t.Errorf("real ring id truncated: got %q (%d bytes), want %q (%d bytes)", got, len(got), want, len(want))
	}
}

func TestSlug(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Remember to order more filament.", "remember-to-order-more-filament"},
		{"../../etc/passwd", "etc-passwd"},
		{"slash dot dot slash etc slash passwd", "slash-dot-dot-slash-etc-slash"},
		{"..", ""},
		{".", ""},
		{"---", ""},
		{"-abc", "abc"},
		{"", ""},
		{"Привет мир", ""}, // non-ASCII speech
		{"café latte", "cafe-latte"},
		{"one two three four five six seven eight", "one-two-three-four-five-six"},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cc", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	} {
		if got := slug(tc.in); got != tc.want {
			t.Errorf("slug(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
	if got := slug("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cc"); len(got) > 60 {
		t.Errorf("slug exceeded 60 bytes: %d", len(got))
	}
}

func TestRenderGolden(t *testing.T) {
	ts := fixedTime
	for _, tc := range []struct {
		name     string
		note     note
		wantFile string
	}{
		{
			"both",
			note{recordedAt: ts, recordingID: "01J8XQ4T7V", hasAudio: true, transcription: "Remember to order more filament.", trigger: "double-click-hold"},
			"2026-08-16 1432 remember-to-order-more-filament.md",
		},
		{
			"transcription-only",
			note{recordedAt: ts, transcription: "Remember to order more filament.", trigger: "single-click-hold"},
			"2026-08-16 1432 remember-to-order-more-filament.md",
		},
		{
			"audio-only",
			note{recordedAt: ts, recordingID: "01J8XQ4T7V", hasAudio: true},
			"2026-08-16 1432.md",
		},
		{
			"neither",
			note{recordedAt: ts, clockServer: true},
			"2026-08-16 1432.md",
		},
		{
			"synthesised",
			note{recordedAt: ts, recordingID: "20260816T143207-c0de", hasAudio: true, idSynthesised: true, transcription: "hello there"},
			"2026-08-16 1432 hello-there.md",
		},
		{
			"truncated",
			note{recordedAt: ts, recordingID: "01J8XQ4T7V", hasAudio: true, truncated: true, clockServer: true},
			"2026-08-16 1432.md",
		},
		{
			"clock-fallback",
			note{recordedAt: ts, clockServer: true, transcription: "late note"},
			"2026-08-16 1432 late-note.md",
		},
		{
			"size-mismatch",
			note{recordedAt: ts, recordingID: "01J8XQ4T7V", hasAudio: true, audioSizeMismatch: true},
			"2026-08-16 1432.md",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, content := tc.note.render()
			if name != tc.wantFile {
				t.Errorf("filename = %q; want %q", name, tc.wantFile)
			}
			golden := filepath.Join("testdata", "golden", tc.name+".md")
			if *update {
				if err := os.WriteFile(golden, content, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != string(want) {
				t.Errorf("content mismatch\n got:\n%s\nwant:\n%s", content, want)
			}
		})
	}
}

func TestParseTokens(t *testing.T) {
	long1 := "0123456789abcdef0123456789abcdef"
	long2 := "fedcba9876543210fedcba9876543210"
	if got, err := parseTokens(long1 + "," + long2); err != nil || len(got) != 2 {
		t.Errorf("two valid tokens: got %d digests, err %v", len(got), err)
	}
	for _, bad := range []string{"", "short", long1 + ",", "," + long1, long1 + ",short"} {
		if _, err := parseTokens(bad); err == nil {
			t.Errorf("parseTokens(%q) succeeded; want error", bad)
		}
	}
}

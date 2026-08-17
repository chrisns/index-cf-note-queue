// Command index-note receives voice notes from a Pebble Index 01 ring and
// writes them into an Obsidian vault. See SPEC.md.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // scratch image has no zoneinfo (SPEC 6.7)
)

var (
	london   *time.Location
	timeNow  = time.Now // swapped in tests
	chmodDir = os.Chmod // swapped in tests
)

type server struct {
	vault          string
	tokenDigests   [][32]byte
	ringPrefixes   []string
	voiceScorerURL string
}

func main() {
	s, listen, err := startup()
	if err != nil {
		log.Fatalf("startup: %v", err)
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           logging(s.routes()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       120 * time.Second,
		WriteTimeout:      150 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    65536,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()
	log.Printf("listening on %s", listen)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	if err := srv.Shutdown(context.Background()); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// startup enforces the contract in SPEC 6.7: fail at deploy, not at 3am.
func startup() (*server, string, error) {
	var err error
	london, err = time.LoadLocation("Europe/London")
	if err != nil {
		return nil, "", fmt.Errorf("load Europe/London: %w", err)
	}
	digests, err := parseTokens(os.Getenv("INDEX_BEARER_TOKENS"))
	if err != nil {
		return nil, "", err
	}
	prefixes, err := parseRingPrefixes(os.Getenv("INDEX_RING_PREFIXES"))
	if err != nil {
		return nil, "", err
	}
	vault := os.Getenv("VAULT_DIR")
	if vault == "" {
		return nil, "", errors.New("VAULT_DIR is not set")
	}
	if err := prepareVault(vault); err != nil {
		return nil, "", err
	}
	listen := os.Getenv("LISTEN_ADDR")
	if listen == "" {
		listen = ":8080"
	}
	// VOICE_SCORER_URL is optional: the voice scorer (VOICE.md) is an
	// out-of-band advisory feature, unlike the bearer and ring prefixes.
	// Unset means no poke, not a startup failure.
	return &server{vault: vault, tokenDigests: digests, ringPrefixes: prefixes, voiceScorerURL: os.Getenv("VOICE_SCORER_URL")}, listen, nil
}

// parseRingPrefixes reads the allowed recording-id prefixes. Each one embeds a
// device identifier, so it is a secret and never belongs in git (SPEC 6.2).
func parseRingPrefixes(env string) ([]string, error) {
	if env == "" {
		return nil, errors.New("INDEX_RING_PREFIXES is not set")
	}
	var out []string
	for _, p := range strings.Split(env, ",") {
		p = strings.TrimSpace(p)
		if len(p) < 8 {
			return nil, fmt.Errorf("INDEX_RING_PREFIXES entry %q is too short to identify a ring", p)
		}
		out = append(out, p)
	}
	return out, nil
}

func parseTokens(env string) ([][32]byte, error) {
	if env == "" {
		return nil, errors.New("INDEX_BEARER_TOKENS is not set")
	}
	var out [][32]byte
	for _, tok := range strings.Split(env, ",") {
		// An empty entry would make "Authorization: Bearer " authenticate (SPEC 6.7).
		if len(tok) < 32 {
			return nil, fmt.Errorf("INDEX_BEARER_TOKENS entry is %d bytes, minimum 32", len(tok))
		}
		out = append(out, sha256.Sum256([]byte(tok)))
	}
	return out, nil
}

func prepareVault(vault string) error {
	info, err := os.Stat(vault)
	if err != nil {
		return fmt.Errorf("VAULT_DIR: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("VAULT_DIR %q is not a directory", vault)
	}
	// Best effort. The provisioner creates this directory as root:root 0777 on
	// NFS, and the pod runs unprivileged with every capability dropped, so
	// chmod is denied. The probe write below is the real gate (SPEC 6.7).
	if err := chmodDir(vault, 0o755); err != nil {
		log.Printf("startup: could not tighten VAULT_DIR mode, continuing: %v", err)
	}
	probe, err := os.CreateTemp(vault, ".probe-*")
	if err != nil {
		return fmt.Errorf("VAULT_DIR probe write: %w", err)
	}
	probe.Close()
	if err := os.Remove(probe.Name()); err != nil {
		return fmt.Errorf("VAULT_DIR probe remove: %w", err)
	}
	year := filepath.Join(vault, timeNow().In(london).Format("2006"))
	for _, dir := range []string{year, filepath.Join(vault, attachmentsDir)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %q: %w", dir, err)
		}
	}
	return nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /note", s.handleNote)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// ServeMux 301-redirects non-canonical paths ("//note"). SPEC 6.1 forbids
	// any redirect, so unknown paths 404 before the mux can canonicalise them.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/note", "/healthz":
			mux.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

type countingReader struct {
	rc io.ReadCloser
	n  int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) Close() error { return c.rc.Close() }

// truncLog caps attacker-controlled strings before they reach a log line
// (SPEC 6.8). Unrelated to the note's truncated flag.
func truncLog(s string) string {
	if len(s) > 256 {
		return s[:256]
	}
	return s
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The kubelet probes /healthz on both replicas every few seconds. Logging
		// that says nothing and buries the requests that matter.
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		cr := &countingReader{rc: r.Body}
		r.Body = cr
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %q %d %d %s ua=%q ct=%q",
			r.Method, truncLog(r.URL.Path), sw.status, cr.n, time.Since(start).Round(time.Millisecond),
			truncLog(r.Header.Get("User-Agent")), truncLog(r.Header.Get("Content-Type")))
	})
}

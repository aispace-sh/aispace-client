package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aispace-sh/aispace-client/internal/config"
)

// rangeServer serves one file with real Range support and can cut a response
// short, which is what an interrupted download looks like from the client side.
type rangeServer struct {
	mu sync.Mutex

	srv     *httptest.Server
	key     string
	content []byte
	// digest is what the metadata advertises; tests can make it disagree with
	// content to exercise the integrity path.
	digest string
	// truncateAfter stops the body early when positive, simulating a drop.
	truncateAfter int
	// ignoreRange emulates a server or proxy that does not honour Range.
	ignoreRange bool
	// rejectRange answers 416, as a server whose copy is shorter would.
	rejectRange bool

	ranges []string
}

func newRangeServer(t *testing.T, content string) *rangeServer {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	s := &rangeServer{
		key:     "ask_validkey0000000000000000000000",
		content: []byte(content),
		digest:  hex.EncodeToString(sum[:]),
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.key {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_key","message":"Invalid key"}}`))
			return
		}
		s.mu.Lock()
		content, digest := append([]byte(nil), s.content...), s.digest
		truncate, ignore, reject := s.truncateAfter, s.ignoreRange, s.rejectRange
		if strings.HasSuffix(r.URL.Path, "/content") {
			s.ranges = append(s.ranges, r.Header.Get("Range"))
		}
		s.mu.Unlock()

		if !strings.HasSuffix(r.URL.Path, "/content") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"01A","name":"big.bin","content_type":"application/octet-stream",`+
				`"size_bytes":%d,"sha256":%q,"visibility":"account","created_at":1,"expires_at":1757604800}`,
				len(content), digest)
			return
		}

		start := 0
		if hdr := r.Header.Get("Range"); hdr != "" && !ignore {
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(hdr, "bytes="), "-"))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if reject || n > len(content) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start = n
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
			w.WriteHeader(http.StatusPartialContent)
		}
		body := content[start:]
		if truncate > 0 && truncate < len(body) {
			body = body[:truncate]
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *rangeServer) rangeHeaders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ranges...)
}

func (s *rangeServer) set(f func(*rangeServer)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

const resumeBody = "0123456789abcdefghijklmnopqrstuvwxyz-the-whole-payload-arrives-eventually"

func setupResume(t *testing.T, body string) (*rangeServer, string) {
	t.Helper()
	isolate(t)
	s := newRangeServer(t, body)
	writeConfig(t, config.File{Key: s.key, URL: s.srv.URL})
	return s, filepath.Join(t.TempDir(), "big.bin")
}

// The whole point: an interrupted transfer leaves a partial file, and rerunning
// the same command continues instead of starting over.
func TestResumeContinuesFromWhereItStopped(t *testing.T) {
	s, out := setupResume(t, resumeBody)
	s.set(func(r *rangeServer) { r.truncateAfter = 20 })

	first := run("", "download", "01A", "--output", out, "--resume")
	if first.code == ExitOK {
		t.Fatalf("truncated transfer should fail: %+v", first)
	}
	info, err := os.Stat(out + partialSuffix)
	if err != nil {
		t.Fatalf("the partial file must survive a failure: %v", err)
	}
	if info.Size() != 20 {
		t.Fatalf("partial is %d bytes, want the 20 that arrived", info.Size())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("an incomplete download must not appear at the destination")
	}

	s.set(func(r *rangeServer) { r.truncateAfter = 0 })
	second := run("", "download", "01A", "--output", out, "--resume")
	if second.code != ExitOK {
		t.Fatalf("resume failed: %+v", second)
	}

	got, err := os.ReadFile(out)
	if err != nil || string(got) != resumeBody {
		t.Fatalf("assembled content = %q (err %v)", got, err)
	}
	// The second run must have asked for the remainder, not the whole file.
	if hs := s.rangeHeaders(); len(hs) < 2 || hs[len(hs)-1] != "bytes=20-" {
		t.Fatalf("range headers = %v, want the last to be bytes=20-", hs)
	}
	for _, leftover := range []string{out + partialSuffix, out + stateSuffix} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s should be gone after finalizing", leftover)
		}
	}
}

// A first run with nothing to resume from must not send a Range header.
func TestResumeFirstRunRequestsWholeFile(t *testing.T) {
	s, out := setupResume(t, resumeBody)
	if r := run("", "download", "01A", "--output", out, "--resume"); r.code != ExitOK {
		t.Fatalf("%+v", r)
	}
	if hs := s.rangeHeaders(); len(hs) != 1 || hs[0] != "" {
		t.Fatalf("range headers = %v, want exactly one empty range", hs)
	}
	got, _ := os.ReadFile(out)
	if string(got) != resumeBody {
		t.Fatalf("content = %q", got)
	}
}

// A partial belonging to different content must never be appended to. Without
// the state file this is undetectable until the digest fails, and only if one
// was recorded.
func TestResumeRejectsPartialFromDifferentContent(t *testing.T) {
	s, out := setupResume(t, resumeBody)
	if err := os.WriteFile(out+partialSuffix, []byte("XXXXXXXXXX"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale, err := json.Marshal(resumeState{FileID: "01A", SizeBytes: 999, SHA256: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out+stateSuffix, stale, 0o600); err != nil {
		t.Fatal(err)
	}

	r := run("", "download", "01A", "--output", out, "--resume")
	if r.code != ExitOK {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.stderr, "different content") {
		t.Errorf("stderr should explain the restart: %q", r.stderr)
	}
	if hs := s.rangeHeaders(); hs[len(hs)-1] != "" {
		t.Errorf("should have restarted without a Range, got %v", hs)
	}
	got, _ := os.ReadFile(out)
	if string(got) != resumeBody {
		t.Fatalf("content = %q; the stale prefix must not survive", got)
	}
}

// A .partial with no state beside it proves nothing about its contents.
func TestResumeRejectsPartialWithoutState(t *testing.T) {
	_, out := setupResume(t, resumeBody)
	if err := os.WriteFile(out+partialSuffix, []byte("XXXXXXXXXX"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := run("", "download", "01A", "--output", out, "--resume")
	if r.code != ExitOK {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.stderr, "no resume state") {
		t.Errorf("stderr = %q", r.stderr)
	}
	got, _ := os.ReadFile(out)
	if string(got) != resumeBody {
		t.Fatalf("content = %q", got)
	}
}

// A server or proxy that ignores Range sends the whole file. Appending that to
// an existing prefix would duplicate it, producing a file of the wrong length
// and wrong content.
func TestResumeHandlesServerIgnoringRange(t *testing.T) {
	s, out := setupResume(t, resumeBody)
	s.set(func(r *rangeServer) { r.truncateAfter = 20 })
	if r := run("", "download", "01A", "--output", out, "--resume"); r.code == ExitOK {
		t.Fatal("expected the truncated run to fail")
	}

	s.set(func(r *rangeServer) { r.truncateAfter = 0; r.ignoreRange = true })
	second := run("", "download", "01A", "--output", out, "--resume")
	if second.code != ExitOK {
		t.Fatalf("%+v", second)
	}
	if !strings.Contains(second.stderr, "ignored the resume range") {
		t.Errorf("stderr should explain the restart: %q", second.stderr)
	}
	got, _ := os.ReadFile(out)
	if string(got) != resumeBody {
		t.Fatalf("content = %q, want the file exactly once", got)
	}
}

// 416 means the server has less content than the saved offset claims.
func TestResumeHandlesRangeRejection(t *testing.T) {
	s, out := setupResume(t, resumeBody)
	s.set(func(r *rangeServer) { r.truncateAfter = 20 })
	if r := run("", "download", "01A", "--output", out, "--resume"); r.code == ExitOK {
		t.Fatal("expected the truncated run to fail")
	}

	s.set(func(r *rangeServer) { r.truncateAfter = 0; r.rejectRange = true })
	second := run("", "download", "01A", "--output", out, "--resume")
	if second.code != ExitOK {
		t.Fatalf("%+v", second)
	}
	if !strings.Contains(second.stderr, "rejected the resume offset") {
		t.Errorf("stderr = %q", second.stderr)
	}
	got, _ := os.ReadFile(out)
	if string(got) != resumeBody {
		t.Fatalf("content = %q", got)
	}
}

// The assembled file is hashed as a whole, not just the bytes one run received.
// Resumed content is exactly what a per-run hash cannot vouch for.
func TestResumeVerifiesTheAssembledFileNotJustTheNewBytes(t *testing.T) {
	s, out := setupResume(t, resumeBody)
	s.set(func(r *rangeServer) { r.truncateAfter = 20 })
	if r := run("", "download", "01A", "--output", out, "--resume"); r.code == ExitOK {
		t.Fatal("expected the truncated run to fail")
	}

	// Corrupt the prefix that already landed. The remaining bytes will arrive
	// intact, so only a hash over the whole file can catch this.
	partial, err := os.ReadFile(out + partialSuffix)
	if err != nil {
		t.Fatal(err)
	}
	partial[0] ^= 0xFF
	if err := os.WriteFile(out+partialSuffix, partial, 0o600); err != nil {
		t.Fatal(err)
	}

	s.set(func(r *rangeServer) { r.truncateAfter = 0 })
	second := run("", "download", "01A", "--output", out, "--resume")
	if second.code != ExitGeneric {
		t.Fatalf("exit = %d, want %d: %+v", second.code, ExitGeneric, second)
	}
	if !strings.Contains(second.stderr, "checksum mismatch") {
		t.Fatalf("stderr = %q", second.stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("a corrupt assembly must not reach the destination")
	}
	if _, err := os.Stat(out + partialSuffix); !os.IsNotExist(err) {
		t.Fatal("a partial known to be corrupt must be discarded, not kept for another resume")
	}
}

// A completed .partial that never got renamed must finalize without
// re-downloading anything.
func TestResumeFinalizesAnAlreadyCompletePartial(t *testing.T) {
	s, out := setupResume(t, resumeBody)
	if err := os.WriteFile(out+partialSuffix, []byte(resumeBody), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(resumeBody))
	state, _ := json.Marshal(resumeState{
		FileID: "01A", SizeBytes: int64(len(resumeBody)), SHA256: hex.EncodeToString(sum[:]),
	})
	if err := os.WriteFile(out+stateSuffix, state, 0o600); err != nil {
		t.Fatal(err)
	}

	r := run("", "download", "01A", "--output", out, "--resume")
	if r.code != ExitOK {
		t.Fatalf("%+v", r)
	}
	for _, h := range s.rangeHeaders() {
		if strings.HasPrefix(h, "bytes=") {
			t.Fatalf("no content request should have been made, saw %v", s.rangeHeaders())
		}
	}
	got, _ := os.ReadFile(out)
	if string(got) != resumeBody {
		t.Fatalf("content = %q", got)
	}
}

func TestResumeRefusesStdout(t *testing.T) {
	_, _ = setupResume(t, resumeBody)
	r := run("", "download", "01A", "--output", "-", "--resume")
	if r.code != ExitUsage {
		t.Fatalf("exit = %d, want %d: %+v", r.code, ExitUsage, r)
	}
}

func TestResumeRefusesWhenDestinationExists(t *testing.T) {
	_, out := setupResume(t, resumeBody)
	if err := os.WriteFile(out, []byte("already here"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := run("", "download", "01A", "--output", out, "--resume")
	if r.code != ExitUsage {
		t.Fatalf("exit = %d, want %d: %+v", r.code, ExitUsage, r)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "already here" {
		t.Fatalf("existing file was modified: %q", got)
	}
}

func TestResumeJSONReportsDigest(t *testing.T) {
	_, out := setupResume(t, resumeBody)
	r := run("", "download", "01A", "--output", out, "--resume", "--json")
	if r.code != ExitOK {
		t.Fatalf("%+v", r)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("stdout %q: %v", r.stdout, err)
	}
	sum := sha256.Sum256([]byte(resumeBody))
	if got["sha256"] != hex.EncodeToString(sum[:]) || got["output"] != out {
		t.Fatalf("json = %v", got)
	}
}

// Without --resume nothing changes: a failure still removes the partial file
// and leaves no state behind.
func TestWithoutResumeAFailedDownloadLeavesNothing(t *testing.T) {
	s, out := setupResume(t, resumeBody)
	s.set(func(r *rangeServer) { r.truncateAfter = 20 })

	// A short body only fails when it is checked, so ask for verification.
	r := run("", "download", "01A", "--output", out, "--verify")
	if r.code == ExitOK {
		t.Fatalf("expected a failure: %+v", r)
	}
	for _, leftover := range []string{out, out + partialSuffix, out + stateSuffix} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s should not exist without --resume", leftover)
		}
	}
}

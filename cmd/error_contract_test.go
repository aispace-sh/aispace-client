package cmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/aispace-sh/aispace-client/internal/config"
)

// errorEnvelope is the documented shape of a --json failure. Decoding into a
// struct rather than a map is deliberate: a renamed field fails here instead of
// being silently absent.
type errorEnvelope struct {
	Error struct {
		Code       string          `json:"code"`
		Message    string          `json:"message"`
		Status     int             `json:"status,omitempty"`
		Details    json.RawMessage `json:"details,omitempty"`
		RetryAfter int             `json:"retry_after,omitempty"`
		ExitCode   int             `json:"exit_code"`
	} `json:"error"`
}

func parseEnvelope(t *testing.T, stderr string) errorEnvelope {
	t.Helper()
	trimmed := strings.TrimSpace(stderr)
	if strings.Count(trimmed, "\n") != 0 {
		t.Fatalf("stderr must be exactly one JSON object, got %d lines: %q",
			strings.Count(trimmed, "\n")+1, stderr)
	}
	var e errorEnvelope
	if err := json.Unmarshal([]byte(trimmed), &e); err != nil {
		t.Fatalf("stderr is not the documented envelope: %v\n%q", err, stderr)
	}
	return e
}

// Each documented exit-code class, produced by a real command against a server
// that returns the real status. These are the examples in docs/CLI.md, so the
// documentation cannot drift from the envelope without failing here.
func TestErrorEnvelopePerExitCodeClass(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		args     []string
		wantExit int
		wantCode string
	}{
		{
			name:     "usage",
			args:     []string{"upload", "-", "--expires", "not-a-duration"},
			wantExit: ExitUsage,
			wantCode: "usage",
		},
		{
			name:     "authentication",
			status:   http.StatusUnauthorized,
			body:     `{"error":{"code":"invalid_key","message":"Invalid key"}}`,
			args:     []string{"quota"},
			wantExit: ExitAuth,
			wantCode: "invalid_key",
		},
		{
			name:     "quota",
			status:   http.StatusPaymentRequired,
			body:     `{"error":{"code":"quota_exceeded","message":"Account allowance exceeded"}}`,
			args:     []string{"quota"},
			wantExit: ExitQuota,
			wantCode: "quota_exceeded",
		},
		{
			name:     "rate limited",
			status:   http.StatusTooManyRequests,
			body:     `{"error":{"code":"rate_limited","message":"Too many requests"}}`,
			args:     []string{"quota"},
			wantExit: ExitRateLimit,
			wantCode: "rate_limited",
		},
		{
			name:     "monthly cap",
			status:   http.StatusTooManyRequests,
			body:     `{"error":{"code":"monthly_upload_cap","message":"Monthly upload cap reached"}}`,
			args:     []string{"quota"},
			wantExit: ExitRateLimit,
			wantCode: "monthly_upload_cap",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			f := newFakeServer(t)
			writeConfig(t, config.File{Key: f.key, URL: f.srv.URL})
			if c.status != 0 {
				f.fail = func(*http.Request) (int, string) { return c.status, c.body }
			}

			args := append(append([]string{}, c.args...), "--json")
			r := run("", args...)

			if r.code != c.wantExit {
				t.Fatalf("exit = %d, want %d: %+v", r.code, c.wantExit, r)
			}
			if r.stdout != "" {
				t.Errorf("stdout must stay empty in --json mode, got %q", r.stdout)
			}
			e := parseEnvelope(t, r.stderr)
			if e.Error.Code != c.wantCode {
				t.Errorf("code = %q, want %q", e.Error.Code, c.wantCode)
			}
			if e.Error.ExitCode != c.wantExit {
				t.Errorf("exit_code = %d but the process exited %d; a caller reading either must get the same answer",
					e.Error.ExitCode, c.wantExit)
			}
			if e.Error.Message == "" {
				t.Error("message must never be empty")
			}
			if c.status != 0 && e.Error.Status != c.status {
				t.Errorf("status = %d, want %d", e.Error.Status, c.status)
			}
		})
	}
}

// leafCommands returns every runnable command, so a contract is checked against
// all of them rather than the handful a test author happened to think of.
func leafCommands(t *testing.T) []*cobra.Command {
	t.Helper()
	var leaves []*cobra.Command
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		subs := c.Commands()
		runnable := true
		for _, sub := range subs {
			if sub.Name() == "help" {
				continue
			}
			runnable = false
			walk(sub)
		}
		if runnable && c.Runnable() && c.Name() != "help" {
			leaves = append(leaves, c)
		}
	}
	walk((&app{}).newRootCmd())
	if len(leaves) < 20 {
		t.Fatalf("only found %d commands; the walk is wrong", len(leaves))
	}
	return leaves
}

func commandPath(c *cobra.Command) []string {
	parts := strings.Fields(c.CommandPath())
	return parts[1:] // drop the root name
}

// An unrecognised flag is a usage error on every command. This is checked by
// walking the tree so a new command cannot quietly return a different code:
// exit 1 would tell an agent the failure is transient and worth retrying.
//
// Parsing fails before RunE, so nothing here contacts a server or touches a
// file.
func TestUnknownFlagIsUsageErrorOnEveryCommand(t *testing.T) {
	isolate(t)
	for _, c := range leafCommands(t) {
		path := commandPath(c)
		t.Run(strings.Join(path, "/"), func(t *testing.T) {
			r := run("", append(append([]string{}, path...), "--definitely-not-a-flag")...)
			if r.code != ExitUsage {
				t.Fatalf("exit = %d, want %d: %+v", r.code, ExitUsage, r)
			}
		})
	}
}

// The --json failure contract, checked on every command: stdout stays empty and
// stderr is exactly one envelope whose exit_code matches the process exit.
func TestJSONErrorContractOnEveryCommand(t *testing.T) {
	isolate(t)
	for _, c := range leafCommands(t) {
		path := commandPath(c)
		t.Run(strings.Join(path, "/"), func(t *testing.T) {
			r := run("", append(append([]string{}, path...), "--definitely-not-a-flag", "--json")...)
			if r.code == ExitOK {
				t.Fatalf("expected a failure, got %+v", r)
			}
			if r.stdout != "" {
				t.Errorf("stdout must stay empty in --json mode, got %q", r.stdout)
			}
			e := parseEnvelope(t, r.stderr)
			if e.Error.ExitCode != r.code {
				t.Errorf("exit_code = %d but the process exited %d", e.Error.ExitCode, r.code)
			}
			if e.Error.Code == "" || e.Error.Message == "" {
				t.Errorf("envelope is incomplete: %q", r.stderr)
			}
		})
	}
}

// The human format is the other half of the contract, and agents parse it too
// when --json is not available on a command.
func TestHumanErrorFormatOnEveryCommand(t *testing.T) {
	isolate(t)
	for _, c := range leafCommands(t) {
		path := commandPath(c)
		t.Run(strings.Join(path, "/"), func(t *testing.T) {
			r := run("", append(append([]string{}, path...), "--definitely-not-a-flag")...)
			if !strings.HasPrefix(r.stderr, "error: ") {
				t.Fatalf("stderr must start with %q: %q", "error: ", r.stderr)
			}
			if !strings.Contains(r.stderr, "(usage)") {
				t.Fatalf("stderr must end with the code in parentheses: %q", r.stderr)
			}
		})
	}
}

// Flag parsing stops at the first unrecognised argument, so --json written
// after a typo was never reached and the error came out in human form. The docs
// put --json last in every example, so that is the order agents copy -- and the
// failure they cannot parse is exactly the one they need to read.
func TestJSONIsHonouredRegardlessOfFlagOrder(t *testing.T) {
	isolate(t)
	cases := []struct {
		name     string
		args     []string
		wantJSON bool
	}{
		{"json after the bad flag", []string{"quota", "--bogus", "--json"}, true},
		{"json before the bad flag", []string{"quota", "--json", "--bogus"}, true},
		{"explicit --json=true", []string{"quota", "--bogus", "--json=true"}, true},
		{"explicit --json=false is respected", []string{"quota", "--bogus", "--json=false"}, false},
		{"no --json at all", []string{"quota", "--bogus"}, false},
		{"after -- it is positional, not a flag", []string{"upload", "--", "--json"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := run("", c.args...)
			if r.code == ExitOK {
				t.Fatalf("expected a failure: %+v", r)
			}
			isJSON := strings.HasPrefix(strings.TrimSpace(r.stderr), "{")
			if isJSON != c.wantJSON {
				t.Fatalf("json=%v, want %v: %q", isJSON, c.wantJSON, r.stderr)
			}
			if c.wantJSON {
				if e := parseEnvelope(t, r.stderr); e.Error.ExitCode != r.code {
					t.Fatalf("exit_code = %d, process exited %d", e.Error.ExitCode, r.code)
				}
			} else if !strings.HasPrefix(r.stderr, "error: ") {
				t.Fatalf("want the human format: %q", r.stderr)
			}
		})
	}
}

package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aispace-sh/aispace-client/internal/config"
)

// doctorServer answers whoami and quota with values a test can bend, so each
// allowance can be exhausted independently.
type doctorServer struct {
	srv       *httptest.Server
	key       string
	revoked   bool
	whoamiErr int
	quota     string
}

const healthyQuota = `{"key":{"budget_bytes":52428800,"used_bytes":1048576,"remaining_bytes":51380224,"budget_limited":true},` +
	`"account":{"allowance_bytes":104857600,"used_bytes":3145728,"remaining_bytes":101711872,"plan":"free","extra_blocks":0},` +
	`"month":{"uploads_used":12,"uploads_limit":100,"downloads_used":340,"downloads_limit":1000,"period_end":1759276800},` +
	`"limits":{"max_file_bytes":26214400,"max_file_ttl_seconds":2592000,"max_link_ttl_seconds":604800,"uploads_per_hour":60,"uploads_per_day":500,"requests_per_minute":300},` +
	`"rate":{"uploads_hour_remaining":58,"uploads_day_remaining":490,"requests_minute_remaining":299}}`

func newDoctorServer(t *testing.T) *doctorServer {
	t.Helper()
	d := &doctorServer{key: "ask_validkey0000000000000000000000", quota: healthyQuota}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+d.key {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_key","message":"Invalid key"}}`))
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/whoami"):
			if d.whoamiErr != 0 {
				w.WriteHeader(d.whoamiErr)
				_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"backend down"}}`))
				return
			}
			revoked := "null"
			if d.revoked {
				revoked = "1757000000"
			}
			fmt.Fprintf(w, `{"key":{"id":"k1","name":"research-bot","prefix":"ask_validkey","budget_bytes":52428800,`+
				`"used_bytes":1048576,"created_at":1,"last_used_at":null,"revoked_at":%s},`+
				`"user":{"email":"luigi@example.com"}}`, revoked)
		case strings.HasSuffix(r.URL.Path, "/quota"):
			_, _ = w.Write([]byte(d.quota))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func doctorJSON(t *testing.T, out string) doctorReport {
	t.Helper()
	var r doctorReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("stdout %q: %v", out, err)
	}
	return r
}

func checkNamed(t *testing.T, r doctorReport, name string) check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %+v", name, r.Checks)
	return check{}
}

func TestDoctorHealthyAccountPassesEverything(t *testing.T) {
	isolate(t)
	d := newDoctorServer(t)
	writeConfig(t, config.File{Key: d.key, URL: d.srv.URL})

	r := run("", "doctor")
	if r.code != ExitOK {
		t.Fatalf("exit = %d, want 0: %+v", r.code, r)
	}
	if !strings.Contains(r.stdout, "all checks passed") {
		t.Fatalf("stdout = %q", r.stdout)
	}
	if strings.Contains(r.stdout+r.stderr, d.key) {
		t.Fatal("the key was printed")
	}
}

// Without a key, doctor must return the same code an authenticated command
// would, so a caller does not need a second set of codes.
func TestDoctorNoKeyExitsAuth(t *testing.T) {
	isolate(t)
	r := run("", "doctor")
	if r.code != ExitAuth {
		t.Fatalf("exit = %d, want %d: %+v", r.code, ExitAuth, r)
	}
	if !strings.Contains(r.stdout, "no API key configured") {
		t.Fatalf("stdout = %q", r.stdout)
	}
	// Checks that cannot run must say so rather than being silently absent.
	got := doctorJSON(t, run("", "doctor", "--json").stdout)
	if c := checkNamed(t, got, "auth"); c.Status != statusSkip {
		t.Fatalf("auth = %+v, want skip", c)
	}
	if c := checkNamed(t, got, "quota"); c.Status != statusSkip {
		t.Fatalf("quota = %+v, want skip", c)
	}
}

func TestDoctorRejectedKeyExitsAuth(t *testing.T) {
	isolate(t)
	d := newDoctorServer(t)
	writeConfig(t, config.File{Key: "ask_wrongkey000000000000000000000", URL: d.srv.URL})

	r := run("", "doctor")
	if r.code != ExitAuth {
		t.Fatalf("exit = %d, want %d: %+v", r.code, ExitAuth, r)
	}
	if !strings.Contains(r.stdout, "copied whole") {
		t.Fatalf("expected an actionable hint: %q", r.stdout)
	}
}

func TestDoctorRevokedKeyExitsAuth(t *testing.T) {
	isolate(t)
	d := newDoctorServer(t)
	d.revoked = true
	writeConfig(t, config.File{Key: d.key, URL: d.srv.URL})

	r := run("", "doctor")
	if r.code != ExitAuth {
		t.Fatalf("exit = %d, want %d: %+v", r.code, ExitAuth, r)
	}
	if !strings.Contains(r.stdout, "revoked") {
		t.Fatalf("stdout = %q", r.stdout)
	}
}

// An environment variable silently outranking a saved config file is the
// setting most often wrong, and the least visible.
func TestDoctorReportsShadowedConfigKey(t *testing.T) {
	isolate(t)
	d := newDoctorServer(t)
	writeConfig(t, config.File{Key: d.key, URL: d.srv.URL})
	t.Setenv(config.EnvKey, d.key)

	got := doctorJSON(t, run("", "doctor", "--json").stdout)
	c := checkNamed(t, got, "config")
	if c.Status != statusWarn {
		t.Fatalf("config = %+v, want warn", c)
	}
	if !strings.Contains(c.Detail, config.EnvKey) || !strings.Contains(c.Hint, "ignored") {
		t.Fatalf("config check should name the shadowing: %+v", c)
	}
	// A shadowed key is still a working key, so nothing should fail.
	if got.Status != statusWarn {
		t.Fatalf("overall status = %q, want warn", got.Status)
	}
}

func TestDoctorBadURLExitsUsage(t *testing.T) {
	isolate(t)
	writeConfig(t, config.File{Key: "ask_validkey0000000000000000000000", URL: "http://example.test"})

	r := run("", "doctor")
	if r.code != ExitUsage {
		t.Fatalf("exit = %d, want %d: %+v", r.code, ExitUsage, r)
	}
	if !strings.Contains(r.stdout, "HTTPS") {
		t.Fatalf("stdout = %q", r.stdout)
	}
}

// Storage and the monthly caps exhaust differently and are fixed differently,
// so they carry the exit code of the failure they predict.
func TestDoctorExhaustedAllowancesUseTheirOwnExitCodes(t *testing.T) {
	tests := []struct {
		name  string
		quota string
		want  int
		check string
	}{
		{
			name: "account storage full",
			quota: strings.Replace(healthyQuota,
				`"remaining_bytes":101711872`, `"remaining_bytes":0`, 1),
			want:  ExitQuota,
			check: "quota-account",
		},
		{
			name: "monthly upload cap reached",
			quota: strings.Replace(healthyQuota,
				`"uploads_used":12`, `"uploads_used":100`, 1),
			want:  ExitRateLimit,
			check: "quota-uploads",
		},
		{
			name: "monthly download cap reached",
			quota: strings.Replace(healthyQuota,
				`"downloads_used":340`, `"downloads_used":1000`, 1),
			want:  ExitRateLimit,
			check: "quota-downloads",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			d := newDoctorServer(t)
			d.quota = tt.quota
			writeConfig(t, config.File{Key: d.key, URL: d.srv.URL})

			r := run("", "doctor")
			if r.code != tt.want {
				t.Fatalf("exit = %d, want %d: %+v", r.code, tt.want, r)
			}
			got := doctorJSON(t, run("", "doctor", "--json").stdout)
			if c := checkNamed(t, got, tt.check); c.Status != statusFail {
				t.Fatalf("%s = %+v, want fail", tt.check, c)
			}
			// Auth succeeded, so it must still be reported as ok.
			if c := checkNamed(t, got, "auth"); c.Status != statusOK {
				t.Fatalf("auth = %+v, want ok; one failure must not hide the rest", c)
			}
		})
	}
}

// A nearly full allowance is worth saying, but it is not a failure and must
// not change the exit code.
func TestDoctorLowAllowanceWarnsWithoutFailing(t *testing.T) {
	isolate(t)
	d := newDoctorServer(t)
	d.quota = strings.Replace(healthyQuota, `"remaining_bytes":101711872`, `"remaining_bytes":1048576`, 1)
	writeConfig(t, config.File{Key: d.key, URL: d.srv.URL})

	r := run("", "doctor")
	if r.code != ExitOK {
		t.Fatalf("exit = %d, want 0 for a warning: %+v", r.code, r)
	}
	got := doctorJSON(t, run("", "doctor", "--json").stdout)
	if c := checkNamed(t, got, "quota-account"); c.Status != statusWarn {
		t.Fatalf("quota-account = %+v, want warn", c)
	}
	if got.Status != statusWarn {
		t.Fatalf("status = %q, want warn", got.Status)
	}
}

// An unreachable server is a network problem, not an auth one.
func TestDoctorUnreachableServerExitsGeneric(t *testing.T) {
	isolate(t)
	d := newDoctorServer(t)
	url := d.srv.URL
	d.srv.Close()
	writeConfig(t, config.File{Key: d.key, URL: url})

	r := run("", "doctor")
	if r.code != ExitGeneric {
		t.Fatalf("exit = %d, want %d: %+v", r.code, ExitGeneric, r)
	}
	got := doctorJSON(t, run("", "doctor", "--json").stdout)
	if c := checkNamed(t, got, "auth"); c.Status != statusFail || !strings.Contains(c.Hint, url) {
		t.Fatalf("auth = %+v, want a failure naming the server", c)
	}
}

func TestDoctorJSONShape(t *testing.T) {
	isolate(t)
	d := newDoctorServer(t)
	writeConfig(t, config.File{Key: d.key, URL: d.srv.URL})

	r := run("", "doctor", "--json")
	if r.code != ExitOK {
		t.Fatalf("%+v", r)
	}
	got := doctorJSON(t, r.stdout)
	if got.Status != statusOK || got.Version == "" || got.UserAgent == "" {
		t.Fatalf("report = %+v", got)
	}
	for _, name := range []string{"config", "config-file", "url", "auth", "quota-key", "quota-account", "quota-uploads", "quota-downloads", "rate"} {
		c := checkNamed(t, got, name)
		if c.Status == "" || c.Detail == "" {
			t.Fatalf("%s is incomplete: %+v", name, c)
		}
	}
	if strings.Contains(r.stdout, d.key) {
		t.Fatal("the key was printed in JSON mode")
	}
}

// The report is the output; the failure must not also be echoed as an error
// line, which would duplicate what is already on stdout.
func TestDoctorDoesNotRepeatTheFailureOnStderr(t *testing.T) {
	isolate(t)
	r := run("", "doctor")
	if r.code != ExitAuth {
		t.Fatalf("%+v", r)
	}
	if strings.Contains(r.stderr, "one or more checks failed") {
		t.Fatalf("failure echoed to stderr: %q", r.stderr)
	}
}

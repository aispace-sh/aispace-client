package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aispace-sh/aispace-client/internal/api"
	"github.com/aispace-sh/aispace-client/internal/config"
)

// Check outcomes, worst last.
const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusSkip = "skip"
	statusFail = "fail"
)

// lowWaterMark is the fraction of a budget left at which a warning is useful:
// enough headroom to finish what is running, not enough to ignore.
const lowWaterMark = 0.10

// check is one diagnosis. Hint is the next action, and is only set when there
// is a concrete one.
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

type doctorReport struct {
	Version   string  `json:"version"`
	UserAgent string  `json:"user_agent"`
	Checks    []check `json:"checks"`
	Status    string  `json:"status"`
}

// doctorRun accumulates checks and the exit code the first failure earns.
type doctorRun struct {
	checks []check
	// exit is the code of the first failing check, so `doctor` reports the same
	// code the failing command itself would have returned.
	exit int
}

func (d *doctorRun) add(c check, exit int) {
	d.checks = append(d.checks, c)
	if c.Status == statusFail && d.exit == ExitOK {
		d.exit = exit
	}
}

func (d *doctorRun) ok(name, detail string) {
	d.add(check{Name: name, Status: statusOK, Detail: detail}, ExitOK)
}

func (d *doctorRun) warn(name, detail, hint string) {
	d.add(check{Name: name, Status: statusWarn, Detail: detail, Hint: hint}, ExitOK)
}

func (d *doctorRun) fail(name, detail, hint string, exit int) {
	d.add(check{Name: name, Status: statusFail, Detail: detail, Hint: hint}, exit)
}

func (d *doctorRun) skip(name, detail string) {
	d.add(check{Name: name, Status: statusSkip, Detail: detail}, ExitOK)
}

// failed reports whether any check has failed so far, so later checks that
// depend on it can be skipped rather than repeating the same error.
func (d *doctorRun) failed() bool {
	return d.exit != ExitOK
}

func (d *doctorRun) status() string {
	worst := statusOK
	for _, c := range d.checks {
		switch c.Status {
		case statusFail:
			return statusFail
		case statusWarn:
			worst = statusWarn
		}
	}
	return worst
}

func (a *app) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check configuration, connectivity, authentication and quota",
		Long: "Runs the checks that explain why a command is failing, in the order a command\n" +
			"would hit them: where the key comes from, whether the server URL is usable,\n" +
			"whether the key is accepted, and whether anything is out of allowance.\n\n" +
			"Every check that can still run does run, so one problem does not hide another.\n" +
			"The exit code is the one the failing command would itself have returned, so a\n" +
			"caller can branch on it without learning a second set of codes. Warnings do not\n" +
			"change the exit code.\n\n" +
			"No file is uploaded and nothing is modified; the key is never printed.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runDoctor(cmd.Context())
		},
	}
}

func (a *app) runDoctor(ctx context.Context) error {
	d := &doctorRun{}
	cfg := a.checkConfig(d)
	a.checkURL(d, cfg)
	c := a.checkAuth(d, ctx, cfg)
	a.checkQuota(d, ctx, c)

	report := doctorReport{
		Version:   a.version,
		UserAgent: a.userAgent(),
		Checks:    d.checks,
		Status:    d.status(),
	}
	if a.jsonOut {
		if err := a.printJSONValue(report); err != nil {
			return err
		}
	} else {
		a.printDoctor(report)
	}
	if d.failed() {
		// The report is the output; re-printing a message would duplicate the
		// failing line that is already on stdout.
		return &reportedError{err: &codedError{
			code: "doctor",
			err:  errors.New("one or more checks failed"),
			exit: d.exit,
		}}
	}
	return nil
}

// checkConfig reports where the key comes from, which is the setting most often
// wrong: an environment variable silently outranks a saved config file.
func (a *app) checkConfig(d *doctorRun) config.Resolved {
	cfg, err := config.Resolve(a.flagKey, a.flagURL)
	if err != nil {
		d.fail("config", err.Error(), "fix or remove the config file", ExitGeneric)
		return cfg
	}
	fileKey := ""
	if f, _, ferr := config.Load(cfg.Path); ferr == nil {
		fileKey = f.Key
	}

	source, shadowed := keySource(a.flagKey, os.Getenv(config.EnvKey), fileKey)
	switch source {
	case "":
		d.fail("config", "no API key configured",
			"run `aispace login --key ask_...` or set "+config.EnvKey, ExitAuth)
	default:
		detail := fmt.Sprintf("key from %s", source)
		if shadowed != "" {
			// Not an error, but it explains a key that "did not take effect".
			d.warn("config", detail,
				fmt.Sprintf("%s is also set and is being ignored", shadowed))
			break
		}
		d.ok("config", detail)
	}

	if _, err := os.Stat(cfg.Path); err != nil {
		d.skip("config-file", fmt.Sprintf("%s (not present)", cfg.Path))
		return cfg
	}
	if len(cfg.Warnings) > 0 {
		d.warn("config-file", cfg.Path, strings.Join(cfg.Warnings, "; "))
		return cfg
	}
	d.ok("config-file", cfg.Path)
	return cfg
}

// keySource names the winning source under the documented precedence, plus the
// lower-precedence source being shadowed, if any.
func keySource(flag, env, file string) (source, shadowed string) {
	switch {
	case flag != "":
		if env != "" {
			return "--key", config.EnvKey
		}
		if file != "" {
			return "--key", "the config file"
		}
		return "--key", ""
	case env != "":
		if file != "" {
			return config.EnvKey, "the config file"
		}
		return config.EnvKey, ""
	case file != "":
		return "the config file", ""
	}
	return "", ""
}

func (a *app) checkURL(d *doctorRun, cfg config.Resolved) {
	if err := validateServerURL(cfg.URL); err != nil {
		d.fail("url", cfg.URL, err.Error(), ExitUsage)
		return
	}
	detail := cfg.URL
	if cfg.URL != config.DefaultURL {
		detail += " (not the default " + config.DefaultURL + ")"
	}
	d.ok("url", detail)
}

// checkAuth is also the connectivity check: reaching /v1/whoami exercises DNS,
// TCP, TLS and the key in one request, and each failure mode reports itself.
func (a *app) checkAuth(d *doctorRun, ctx context.Context, cfg config.Resolved) *api.Client {
	if d.failed() {
		d.skip("auth", "skipped while an earlier check is failing")
		return nil
	}
	if err := validateKey(cfg.Key); err != nil {
		d.fail("auth", "the configured key is unusable", err.Error(), ExitUsage)
		return nil
	}
	c := api.New(cfg.URL, cfg.Key, a.userAgent())
	c.Sleep = sleep
	if inactivityTimeout > 0 {
		c.InactivityTimeout = inactivityTimeout
	}
	res, err := c.Whoami(ctx)
	if err != nil {
		var ae *api.Error
		if errors.As(err, &ae) && ae.Status != 0 {
			d.fail("auth", ae.Message, authHint(ae), ae.ExitCode())
			return nil
		}
		// No HTTP status means nothing answered: DNS, TCP or TLS, not the key.
		// Saying "auth failed" here would send the reader after the wrong thing.
		d.fail("auth", errorMessage(err), "could not reach "+cfg.URL+"; check the network, proxy settings and the URL", ExitGeneric)
		return nil
	}
	k := res.Value.Key
	if k.RevokedAt != nil {
		d.fail("auth", fmt.Sprintf("key %q (%s) is revoked", k.Name, k.Prefix),
			"create a new key and run `aispace login --key ask_...`", ExitAuth)
		return nil
	}
	d.ok("auth", fmt.Sprintf("%s (%s) for %s", k.Name, k.Prefix, res.Value.User.Email))
	return c
}

// errorMessage prefers the API error message, which already omits any query
// string, over the raw transport error.
func errorMessage(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Message
	}
	return err.Error()
}

func authHint(e *api.Error) string {
	switch e.Code {
	case "invalid_key":
		return "the key was rejected; check it was copied whole and has not been deleted"
	case "key_revoked":
		return "create a new key and run `aispace login --key ask_...`"
	}
	return ""
}

// checkQuota reports every allowance separately, because they fail for
// different reasons and have different remedies.
func (a *app) checkQuota(d *doctorRun, ctx context.Context, c *api.Client) {
	if c == nil {
		d.skip("quota", "skipped without a working key")
		return
	}
	res, err := c.Quota(ctx)
	if err != nil {
		var ae *api.Error
		if errors.As(err, &ae) {
			d.fail("quota", ae.Message, "", ae.ExitCode())
			return
		}
		d.fail("quota", err.Error(), "", ExitGeneric)
		return
	}
	q := res.Value

	if q.Key.BudgetLimited {
		a.reportBytes(d, "quota-key", q.Key.RemainingBytes, q.Key.BudgetBytes,
			"delete files with `aispace rm`, or raise the key budget in the dashboard")
	} else {
		d.ok("quota-key", fmt.Sprintf("%s used, no key budget", fmtBytes(q.Key.UsedBytes)))
	}
	a.reportBytes(d, "quota-account", q.Account.RemainingBytes, q.Account.AllowanceBytes,
		"free space with `aispace rm`, or add capacity in the dashboard")

	a.reportCount(d, "quota-uploads", q.Month.UploadsUsed, q.Month.UploadsLimit,
		"monthly upload", q.Month.PeriodEnd)
	a.reportCount(d, "quota-downloads", q.Month.DownloadsUsed, q.Month.DownloadsLimit,
		"monthly download", q.Month.PeriodEnd)

	if q.Rate.UploadsHourRemaining == 0 {
		d.warn("rate", "no uploads left this hour", "wait for the next hour, or batch into one archive")
		return
	}
	d.ok("rate", fmt.Sprintf("%d uploads left this hour, %d today, %d requests this minute",
		q.Rate.UploadsHourRemaining, q.Rate.UploadsDayRemaining, q.Rate.RequestsMinuteRemaining))
}

func (a *app) reportBytes(d *doctorRun, name string, remaining, total int64, hint string) {
	detail := fmt.Sprintf("%s of %s remaining", fmtBytes(remaining), fmtBytes(total))
	switch {
	case remaining <= 0:
		d.fail(name, detail, hint, ExitQuota)
	case total > 0 && float64(remaining)/float64(total) < lowWaterMark:
		d.warn(name, detail, hint)
	default:
		d.ok(name, detail)
	}
}

// reportCount covers the monthly caps, which exhaust as a count rather than a
// size and reset on a date rather than by deleting anything.
func (a *app) reportCount(d *doctorRun, name string, used, limit int64, what string, periodEnd int64) {
	if limit <= 0 {
		d.ok(name, fmt.Sprintf("%d used, no %s cap", used, what))
		return
	}
	left := limit - used
	detail := fmt.Sprintf("%d of %d %ss left, resets %s", left, limit, what, fmtTime(periodEnd))
	hint := "nothing frees this before the reset; upgrade the plan to raise the cap"
	switch {
	case left <= 0:
		// The server reports this as 429 monthly_*_cap, which is exit 5.
		d.fail(name, fmt.Sprintf("%s cap reached (%d/%d), resets %s", what, used, limit, fmtTime(periodEnd)),
			hint, ExitRateLimit)
	case float64(left)/float64(limit) < lowWaterMark:
		d.warn(name, detail, hint)
	default:
		d.ok(name, detail)
	}
}

func (a *app) printDoctor(r doctorReport) {
	fmt.Fprintf(a.stdout, "aispace %s (%s/%s)\n\n", r.Version, runtime.GOOS, runtime.GOARCH)
	width := 0
	for _, c := range r.Checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	for _, c := range r.Checks {
		fmt.Fprintf(a.stdout, "%-4s  %-*s  %s\n", c.Status, width, c.Name, c.Detail)
		if c.Hint != "" {
			fmt.Fprintf(a.stdout, "      %-*s  -> %s\n", width, "", c.Hint)
		}
	}
	fails, warns := 0, 0
	for _, c := range r.Checks {
		switch c.Status {
		case statusFail:
			fails++
		case statusWarn:
			warns++
		}
	}
	fmt.Fprintf(a.stdout, "\n%s\n", summarize(fails, warns))
}

func summarize(fails, warns int) string {
	if fails == 0 && warns == 0 {
		return "all checks passed"
	}
	parts := []string{}
	if fails > 0 {
		parts = append(parts, fmt.Sprintf("%s failing", plural(fails, "check")))
	}
	if warns > 0 {
		parts = append(parts, fmt.Sprintf("%s", plural(warns, "warning")))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

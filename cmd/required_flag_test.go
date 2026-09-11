package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// A missing required value is a usage error. cobra's MarkFlagRequired reports
// it as a plain error, which classifies as exit 1 — the code documented for
// network faults and 5xx, so a caller following the contract retries a command
// that can never succeed.
func TestMissingRequiredFlagIsUsageError(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"identity create", []string{"identity", "create"}, "--name"},
		{"identity create with name only", []string{"identity", "create", "--name", "probe"}, "--handle"},
		{"identity rotate", []string{"identity", "rotate", "someid"}, "--purpose"},
		{"recipient verify", []string{"recipient", "verify", "somealias"}, "--fingerprint"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			r := run("", c.args...)
			if r.code != ExitUsage {
				t.Fatalf("exit = %d, want %d: %+v", r.code, ExitUsage, r)
			}
			if !strings.Contains(r.stderr, "(usage)") {
				t.Errorf("stderr should be coded as a usage error: %q", r.stderr)
			}
			if !strings.Contains(r.stderr, c.want) {
				t.Errorf("stderr should name %s: %q", c.want, r.stderr)
			}
			// The generic message replaced the specific one that each command
			// already had; make sure it has not come back.
			if strings.Contains(r.stderr, "required flag(s)") {
				t.Errorf("cobra's generic message is being used: %q", r.stderr)
			}
		})
	}
}

// Structural guard: MarkFlagRequired short-circuits before RunE, so the
// specific check a command already has never runs and the exit code is wrong.
// Commands validate in RunE with usagef instead, which is what every older
// command here does. This catches a reintroduction anywhere in the tree.
func TestNoFlagUsesCobraRequiredAnnotation(t *testing.T) {
	var offenders []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		check := func(f *pflag.Flag) {
			if _, ok := f.Annotations[cobra.BashCompOneRequiredFlag]; ok {
				offenders = append(offenders, c.CommandPath()+" --"+f.Name)
			}
		}
		c.Flags().VisitAll(check)
		c.PersistentFlags().VisitAll(check)
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk((&app{}).newRootCmd())

	if len(offenders) > 0 {
		t.Fatalf("these flags are marked required through cobra, which exits 1 instead of %d:\n  %s\n"+
			"validate in RunE with usagef instead", ExitUsage, strings.Join(offenders, "\n  "))
	}
}

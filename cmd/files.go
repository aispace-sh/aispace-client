package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aispace-sh/aispace-client/internal/api"
	"github.com/aispace-sh/aispace-client/internal/duration"
)

func (a *app) downloadCmd() *cobra.Command {
	var output string
	var verify bool
	var resume bool
	cmd := &cobra.Command{
		Use:   "download <file_id> --output <path|-> [--verify] [--resume]",
		Short: "Download an accessible file by ID without creating a public link",
		Long: "Downloads a file this key owns or that is shared with the account, using key\n" +
			"authentication rather than a public link.\n\n" +
			"With --verify the bytes are hashed as they are written and compared with the\n" +
			"SHA-256 the API records for every file. That costs one extra metadata request.\n" +
			"A response without a digest is rejected as incomplete server metadata.\n\n" +
			"With --resume the bytes land in <output>.partial and are renamed into place only\n" +
			"once the whole file has arrived and matched its recorded digest. A failed run\n" +
			"keeps the partial file, so rerunning the same command continues from where it\n" +
			"stopped instead of starting the transfer again.",
		Example: "  aispace download 01J8ZQ3V9N7X2K4M6P8R0T2W4Y --output report.pdf --verify\n" +
			"  aispace download 01J8ZQ3V9N7X2K4M6P8R0T2W4Y --output big.zip --resume",
		Args: exactArgs(1, "<file_id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				return usagef("--output is required")
			}
			if output == "-" && a.jsonOut {
				return usagef("--json cannot be used with --output -")
			}
			if resume && output == "-" {
				return usagef("--resume needs a file to continue; it cannot be used with --output -")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			id := args[0]

			if resume {
				return a.runResumableDownload(cmd.Context(), c, id, output, verify)
			}

			// Read the recorded digest before transferring anything: without one
			// there is nothing to compare against, and saying so now avoids
			// downloading a body that could never be checked.
			var want string
			if verify {
				res, err := c.GetFile(cmd.Context(), id)
				if err != nil {
					return err
				}
				if want = res.Value.SHA256; want == "" {
					return &codedError{
						code: "no_checksum",
						err:  fmt.Errorf("file %s has no recorded SHA-256 to verify against; the server returned incomplete metadata", id),
						exit: ExitGeneric,
					}
				}
			}

			resp, err := c.Download(cmd.Context(), id)
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			sum := sha256.New()
			body := io.Reader(resp.Body)
			if verify {
				body = io.TeeReader(resp.Body, sum)
			}

			if output == "-" {
				if _, err := io.Copy(a.stdout, body); err != nil {
					return &codedError{code: "io", err: err, exit: ExitGeneric}
				}
				if !verify {
					return nil
				}
				// stdout cannot be taken back, so the exit code is the only
				// signal left; say so rather than implying the bytes were held.
				return checkDigest(want, sum, "the bytes were already written to stdout")
			}

			out, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				if errors.Is(err, os.ErrExist) {
					return usagef("output file already exists: %s", output)
				}
				return &codedError{code: "io", err: err, exit: ExitGeneric}
			}
			ok := false
			defer func() {
				_ = out.Close()
				if !ok {
					_ = os.Remove(output)
				}
			}()
			if _, err = io.Copy(out, body); err != nil {
				return &codedError{code: "io", err: err, exit: ExitGeneric}
			}
			if err = out.Close(); err != nil {
				return &codedError{code: "io", err: err, exit: ExitGeneric}
			}
			if verify {
				// ok is still false here, so the deferred cleanup removes a
				// corrupt file rather than leaving it where it may be trusted.
				if err := checkDigest(want, sum, ""); err != nil {
					return err
				}
			}
			ok = true
			if a.jsonOut {
				payload := map[string]string{"file_id": id, "output": output}
				if verify {
					payload["sha256"] = hex.EncodeToString(sum.Sum(nil))
				}
				return a.printJSONValue(payload)
			}
			if verify {
				fmt.Fprintf(a.stdout, "downloaded %s -> %s sha256 verified\n", id, output)
				return nil
			}
			fmt.Fprintf(a.stdout, "downloaded %s -> %s\n", id, output)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "destination path, or - for stdout (required)")
	cmd.Flags().BoolVar(&verify, "verify", false, "check the downloaded bytes against the file's recorded SHA-256 (one extra API call)")
	cmd.Flags().BoolVar(&resume, "resume", false, "continue an interrupted download from <output>.partial")
	return cmd
}

// checkDigest compares a streamed hash with the digest the API recorded for the
// file. note describes anything the caller could no longer take back.
func checkDigest(want string, h hash.Hash, note string) error {
	got := hex.EncodeToString(h.Sum(nil))
	if strings.EqualFold(got, want) {
		return nil
	}
	msg := fmt.Sprintf("checksum mismatch: recorded %s, downloaded %s", want, got)
	if note != "" {
		msg += " (" + note + ")"
	}
	return &codedError{code: "checksum_mismatch", err: errors.New(msg), exit: ExitGeneric}
}

func (a *app) linkCmd() *cobra.Command {
	var expires string
	var maxDownloads int64
	cmd := &cobra.Command{
		Use:   "link <file_id> [--expires 1h] [--max-downloads N]",
		Short: "Create a Pro public share link; the URL is printed last on its own line",
		Args:  exactArgs(1, "<file_id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if maxDownloads < 0 {
				return usagef("--max-downloads must be >= 0")
			}
			var expiresIn int64
			if expires != "" {
				var err error
				if expiresIn, err = duration.Seconds(expires); err != nil {
					return usagef("--expires: %v", err)
				}
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			res, err := c.CreateLink(cmd.Context(), args[0], api.LinkOptions{ExpiresIn: expiresIn, MaxDownloads: maxDownloads})
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.printJSON(res.Raw)
				return nil
			}
			a.printLinkLine(res.Value)
			fmt.Fprintln(a.stdout, res.Value.URL)
			return nil
		},
	}
	cmd.Flags().StringVar(&expires, "expires", "", "link lifetime, e.g. 30m, 1h, 2d (default: server default, 1h)")
	cmd.Flags().Int64Var(&maxDownloads, "max-downloads", 0, "download cap (default: unlimited)")
	return cmd
}

func (a *app) lsCmd() *cobra.Command {
	var all bool
	var limit int
	var cursor string
	cmd := &cobra.Command{
		Use:   "ls [--limit N] [--cursor C]",
		Short: "List files uploaded with this key",
		Long: "Lists files for the key. By default it follows pagination to the end. Human output\n" +
			"is one line per file: <id> <size> <expires> <name>.\n\n" +
			"--limit stops after N files instead of walking the whole account, which costs\n" +
			"fewer requests on a large one. When more files remain, --json reports the cursor\n" +
			"to resume from in next_cursor; pass it back with --cursor to continue.",
		Example: "  aispace ls --limit 50 --json\n" +
			"  aispace ls --limit 50 --cursor \"$(aispace ls --limit 50 --json | jq -r .next_cursor)\" --json",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("limit") && limit < 1 {
				return usagef("--limit must be >= 1")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			if limit > 0 {
				raws, files, next, err := c.ListPage(cmd.Context(), cursor, limit)
				if err != nil {
					return err
				}
				return a.printFileList(raws, files, next, limit)
			}
			if a.jsonOut {
				var raws []json.RawMessage
				err := c.WalkFilesFrom(cmd.Context(), cursor, func(page []json.RawMessage, _ []api.File) error {
					raws = append(raws, page...)
					return nil
				})
				if err != nil {
					return err
				}
				return a.printFileList(raws, nil, "", 0)
			}
			count := 0
			err = c.WalkFilesFrom(cmd.Context(), cursor, func(_ []json.RawMessage, files []api.File) error {
				for _, f := range files {
					a.printListLine(f)
					count++
				}
				return nil
			})
			if err != nil {
				return err
			}
			if count == 0 {
				fmt.Fprintln(a.stderr, "no files")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "accepted for compatibility; ls follows pagination to the end unless --limit is given")
	cmd.Flags().IntVar(&limit, "limit", 0, "stop after N files instead of listing the whole account")
	cmd.Flags().StringVar(&cursor, "cursor", "", "resume from a next_cursor returned by an earlier --limit run")
	return cmd
}

// printFileList renders a listing in whichever mode is active. next is the
// cursor to resume from, empty when the listing reached the end.
func (a *app) printFileList(raws []json.RawMessage, files []api.File, next string, limit int) error {
	if a.jsonOut {
		if raws == nil {
			raws = []json.RawMessage{}
		}
		var cursor *string
		if next != "" {
			cursor = &next
		}
		return a.printJSONValue(struct {
			Files      []json.RawMessage `json:"files"`
			NextCursor *string           `json:"next_cursor"`
		}{Files: raws, NextCursor: cursor})
	}
	for _, f := range files {
		a.printListLine(f)
	}
	if len(files) == 0 {
		fmt.Fprintln(a.stderr, "no files")
	}
	if next != "" {
		// stderr, so stdout stays one line per file for a pipeline.
		fmt.Fprintf(a.stderr, "more files remain; continue with --limit %d --cursor %s\n", limit, next)
	}
	return nil
}

// printListLine renders one ls row.
func (a *app) printListLine(f api.File) {
	fmt.Fprintf(a.stdout, "%s %s %s %s\n", f.ID, fmtBytes(f.SizeBytes), fmtTime(f.ExpiresAt), f.Name)
}

func (a *app) infoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <file_id>",
		Short: "Show file metadata",
		Args:  exactArgs(1, "<file_id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			res, err := c.GetFile(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.printJSON(res.Raw)
				return nil
			}
			f := res.Value
			sum := f.SHA256
			if sum == "" {
				sum = "-"
			}
			enc := f.EncAlg
			if enc == "" {
				enc = "none"
			}
			visibility := f.Visibility
			if visibility == "" {
				visibility = "unknown"
			}
			fmt.Fprintf(a.stdout, "%s %s %s %s created %s expires %s sha256 %s encryption %s visibility %s\n", f.ID, f.Name, fmtBytes(f.SizeBytes), f.ContentType, fmtTime(f.CreatedAt), fmtTime(f.ExpiresAt), sum, enc, visibility)
			return nil
		},
	}
}

func (a *app) linksCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "links <file_id>",
		Short: "List share links for a file",
		Long:  "Lists share links for a file, one per line: <id> <expires> <downloads>/<max> [revoked].\nURLs are only available at creation time (see `aispace link`).",
		Args:  exactArgs(1, "<file_id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			res, err := c.ListLinks(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.printJSON(res.Raw)
				return nil
			}
			if len(res.Value.Links) == 0 {
				fmt.Fprintln(a.stderr, "no links")
				return nil
			}
			for _, l := range res.Value.Links {
				max := "unlimited"
				if l.MaxDownloads != nil {
					max = fmt.Sprintf("%d", *l.MaxDownloads)
				}
				state := ""
				if l.RevokedAt != nil {
					state = " revoked"
				}
				fmt.Fprintf(a.stdout, "%s expires %s downloads %d/%s%s\n", l.ID, fmtTime(l.ExpiresAt), l.DownloadCount, max, state)
			}
			return nil
		},
	}
}

func (a *app) rmCmd() *cobra.Command {
	var keepGoing bool
	cmd := &cobra.Command{
		Use:   "rm <file_id>... [--continue]",
		Short: "Delete one or more files (revokes all their links)",
		Long: "Deletes files in order. By default it stops at the first failure, so the IDs after\n" +
			"it are left alone. With --continue every ID is attempted and the command still\n" +
			"exits non-zero if any of them failed, which suits cleaning up a list that may\n" +
			"contain IDs already deleted or expired.\n\n" +
			"With --json it prints one {" + `"deleted"` + ": id} object per line.",
		Args: minArgs(1, "<file_id>..."),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			return a.runBatch(args, keepGoing, "deleted", func(id string) error {
				return c.DeleteFile(cmd.Context(), id)
			})
		},
	}
	cmd.Flags().BoolVar(&keepGoing, "continue", false, "attempt every ID instead of stopping at the first failure")
	return cmd
}

func (a *app) revokeCmd() *cobra.Command {
	var keepGoing bool
	cmd := &cobra.Command{
		Use:   "revoke <link_id>... [--continue]",
		Short: "Revoke one or more share links",
		Long: "Revokes links in order. By default it stops at the first failure; with --continue\n" +
			"every ID is attempted and the command still exits non-zero if any of them failed.\n\n" +
			"With --json it prints one {" + `"revoked"` + ": id} object per line.",
		Args: minArgs(1, "<link_id>..."),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			return a.runBatch(args, keepGoing, "revoked", func(id string) error {
				return c.RevokeLink(cmd.Context(), id)
			})
		},
	}
	cmd.Flags().BoolVar(&keepGoing, "continue", false, "attempt every ID instead of stopping at the first failure")
	return cmd
}

// runBatch applies op to each ID, reporting one line per success. Without
// keepGoing it returns at the first failure, leaving the remaining IDs
// untouched. With keepGoing it reports each failure as it happens and returns
// the first one at the end, so the exit code still reflects that something
// failed.
func (a *app) runBatch(ids []string, keepGoing bool, verb string, op func(string) error) error {
	multi := len(ids) > 1
	var first error
	for _, id := range ids {
		if err := op(id); err != nil {
			err = prefixID(id, multi, err)
			if !keepGoing || batchIsHopeless(err) {
				// Earlier recoverable failures were already reported. Return the
				// current fatal error so its message and stronger exit code win.
				return err
			}
			_, code := a.classify(err)
			a.printError(err, code)
			if first == nil {
				first = &reportedError{err: err}
			}
			continue
		}
		if a.jsonOut {
			if err := a.printJSONValue(map[string]any{verb: id}); err != nil {
				return err
			}
			continue
		}
		fmt.Fprintf(a.stdout, "%s %s\n", verb, id)
	}
	return first
}

// batchIsHopeless reports whether an error will affect every remaining ID, so
// there is nothing to gain from trying them. A rejected key or an exhausted
// rate limit applies to the whole batch; a missing ID does not.
func batchIsHopeless(err error) bool {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Status == http.StatusUnauthorized || ae.Status == http.StatusTooManyRequests
	}
	return false
}

func (a *app) quotaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "quota",
		Short: "Show key budget, account allowance, limits and rate remaining",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			res, err := c.Quota(cmd.Context())
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.printJSON(res.Raw)
				return nil
			}
			q := res.Value
			fmt.Fprintf(a.stdout, "key: used %s of %s, %s remaining\n", fmtBytes(q.Key.UsedBytes), fmtBytes(q.Key.BudgetBytes), fmtBytes(q.Key.RemainingBytes))
			fmt.Fprintf(a.stdout, "account: used %s of %s, %s remaining, plan %s (extra blocks %d)\n", fmtBytes(q.Account.UsedBytes), fmtBytes(q.Account.AllowanceBytes), fmtBytes(q.Account.RemainingBytes), q.Account.Plan, q.Account.ExtraBlocks)
			fmt.Fprintf(a.stdout, "month: uploads %d/%d, downloads %d/%d, resets %s\n", q.Month.UploadsUsed, q.Month.UploadsLimit, q.Month.DownloadsUsed, q.Month.DownloadsLimit, fmtTime(q.Month.PeriodEnd))
			fmt.Fprintf(a.stdout, "limits: max file %s, max file ttl %s, max link ttl %s, uploads %d/h %d/d, requests %d/min\n", fmtBytes(q.Limits.MaxFileBytes), fmtSeconds(q.Limits.MaxFileTTLSeconds), fmtSeconds(q.Limits.MaxLinkTTLSeconds), q.Limits.UploadsPerHour, q.Limits.UploadsPerDay, q.Limits.RequestsPerMinute)
			fmt.Fprintf(a.stdout, "rate: uploads remaining %d this hour, %d today; requests remaining %d this minute\n", q.Rate.UploadsHourRemaining, q.Rate.UploadsDayRemaining, q.Rate.RequestsMinuteRemaining)
			return nil
		},
	}
}

// prefixID annotates an API error with the ID it concerns when several were
// given, keeping the error type (and therefore the exit code) intact.
func prefixID(id string, multi bool, err error) error {
	if !multi {
		return err
	}
	var ae *api.Error
	if errors.As(err, &ae) {
		cp := *ae
		cp.Message = id + ": " + ae.Message
		return &cp
	}
	return fmt.Errorf("%s: %w", id, err)
}

func fmtSeconds(s int64) string {
	switch {
	case s%86400 == 0 && s != 0:
		return fmt.Sprintf("%dd", s/86400)
	case s%3600 == 0 && s != 0:
		return fmt.Sprintf("%dh", s/3600)
	case s%60 == 0 && s != 0:
		return fmt.Sprintf("%dm", s/60)
	}
	return fmt.Sprintf("%ds", s)
}

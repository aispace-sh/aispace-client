package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aispace-sh/aispace-client/internal/api"
)

// partialSuffix is appended to the destination while bytes are still arriving,
// so an incomplete download is never mistaken for a finished file.
const partialSuffix = ".partial"

// stateSuffix holds the metadata that binds a .partial file to one version of
// one file. Without it, resuming could append the tail of one file to the head
// of another and produce a plausible-looking result that only a checksum would
// catch -- and only if the caller asked for one.
const stateSuffix = ".partial.json"

// resumeState is written next to the .partial file after every successful
// start, and re-read before resuming.
type resumeState struct {
	FileID      string `json:"file_id"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// matches reports whether a saved state describes the same bytes the server is
// offering now. A file that was deleted and re-uploaded under the same ID has a
// different size or digest, and must not be resumed into.
func (s resumeState) matches(id string, f api.File) bool {
	if s.FileID != id || s.SizeBytes != f.SizeBytes {
		return false
	}
	// An empty digest on either side means the server never recorded one, so
	// there is nothing to compare; size and ID still have to agree.
	if s.SHA256 != "" && f.SHA256 != "" && !strings.EqualFold(s.SHA256, f.SHA256) {
		return false
	}
	return true
}

func partialPath(output string) string { return output + partialSuffix }
func statePath(output string) string   { return output + stateSuffix }

func writeResumeState(output string, s resumeState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(output), append(b, '\n'), 0o600)
}

func readResumeState(output string) (resumeState, bool) {
	b, err := os.ReadFile(statePath(output))
	if err != nil {
		return resumeState{}, false
	}
	var s resumeState
	if err := json.Unmarshal(b, &s); err != nil {
		return resumeState{}, false
	}
	return s, true
}

// discardPartial removes both halves of the resume state. They are only useful
// together, so a leftover of either one would be misleading.
func discardPartial(output string) {
	_ = os.Remove(partialPath(output))
	_ = os.Remove(statePath(output))
}

// resumeOffset decides where a download should start.
//
// It returns 0 whenever anything about the saved state is unusable, because
// restarting costs bandwidth while resuming the wrong bytes costs correctness.
// The reason is returned so the caller can say why the work is being repeated
// rather than appearing to ignore an existing file.
func resumeOffset(output, id string, f api.File) (offset int64, reason string) {
	info, err := os.Stat(partialPath(output))
	if err != nil {
		return 0, ""
	}
	saved, ok := readResumeState(output)
	if !ok {
		return 0, "no resume state alongside it"
	}
	if !saved.matches(id, f) {
		return 0, "it belongs to different content"
	}
	switch {
	case info.Size() > f.SizeBytes:
		return 0, "it is larger than the file on the server"
	case info.Size() == f.SizeBytes:
		// Everything arrived last time; the transfer just never finalized.
		return info.Size(), ""
	}
	return info.Size(), ""
}

// rangeHeader is the HTTP Range value for continuing from offset.
func rangeHeader(offset int64) string {
	if offset <= 0 {
		return ""
	}
	return fmt.Sprintf("bytes=%d-", offset)
}

// offsetPastEnd reports whether an error is the server refusing a range that
// starts beyond the file, which means the saved state disagrees with reality.
func offsetPastEnd(err error) bool {
	var ae *api.Error
	return errors.As(err, &ae) && ae.Status == httpStatusRangeNotSatisfiable
}

const httpStatusRangeNotSatisfiable = 416

// runResumableDownload downloads into <output>.partial and only renames it into
// place once the whole file has arrived and matched its recorded digest.
//
// A failure leaves both the .partial and its state behind on purpose: that is
// the entire point, and it is why the non-resume path, which deletes a partial
// download, is left alone.
func (a *app) runResumableDownload(ctx context.Context, c *api.Client, id, output string, verify bool) error {
	if _, err := os.Stat(output); err == nil {
		return usagef("output file already exists: %s", output)
	}

	meta, err := c.GetFile(ctx, id)
	if err != nil {
		return err
	}
	file := meta.Value

	offset, reason := resumeOffset(output, id, file)
	if reason != "" {
		fmt.Fprintf(a.stderr, "warning: ignoring %s because %s; starting again\n", partialPath(output), reason)
		discardPartial(output)
		offset = 0
	}
	if verify && file.SHA256 == "" {
		return &codedError{
			code: "no_checksum",
			err:  fmt.Errorf("file %s has no recorded SHA-256 to verify against; the server returned incomplete metadata", id),
			exit: ExitGeneric,
		}
	}
	if err := writeResumeState(output, resumeState{
		FileID: id, SizeBytes: file.SizeBytes, SHA256: file.SHA256, ContentType: file.ContentType,
	}); err != nil {
		return &codedError{code: "io", err: fmt.Errorf("save resume state: %w", err), exit: ExitGeneric}
	}

	if offset > 0 && offset < file.SizeBytes {
		fmt.Fprintf(a.stderr, "resuming %s at %s of %s\n", output, fmtBytes(offset), fmtBytes(file.SizeBytes))
	}
	if offset < file.SizeBytes {
		if offset, err = a.appendRemainder(ctx, c, id, output, offset); err != nil {
			fmt.Fprintf(a.stderr, "kept %s; rerun the same command to continue from %s\n",
				partialPath(output), fmtBytes(offset))
			return err
		}
	}

	if offset != file.SizeBytes {
		return &codedError{
			code: "short_transfer",
			err: fmt.Errorf("expected %d bytes but assembled %d; kept %s",
				file.SizeBytes, offset, partialPath(output)),
			exit: ExitGeneric,
		}
	}

	// Hash the assembled file rather than only the bytes this run received.
	// Resumed content is precisely what a per-run hash cannot vouch for, so
	// checking the whole file is what keeps resume from weakening the check.
	digest := ""
	if file.SHA256 != "" {
		if digest, err = hashFileOnDisk(partialPath(output)); err != nil {
			return &codedError{code: "io", err: fmt.Errorf("hash %s: %w", partialPath(output), err), exit: ExitGeneric}
		}
		if !strings.EqualFold(digest, file.SHA256) {
			discardPartial(output)
			return &codedError{
				code: "checksum_mismatch",
				err: fmt.Errorf("checksum mismatch: recorded %s, assembled %s (discarded %s)",
					file.SHA256, digest, partialPath(output)),
				exit: ExitGeneric,
			}
		}
	} else {
		fmt.Fprintf(a.stderr, "warning: %s has no recorded SHA-256, so the assembled file could not be verified\n", id)
	}

	if err := os.Rename(partialPath(output), output); err != nil {
		return &codedError{code: "io", err: fmt.Errorf("finalize %s: %w", output, err), exit: ExitGeneric}
	}
	_ = os.Remove(statePath(output))

	if a.jsonOut {
		payload := map[string]string{"file_id": id, "output": output}
		if digest != "" {
			payload["sha256"] = digest
		}
		return a.printJSONValue(payload)
	}
	if digest != "" {
		fmt.Fprintf(a.stdout, "downloaded %s -> %s sha256 verified\n", id, output)
		return nil
	}
	fmt.Fprintf(a.stdout, "downloaded %s -> %s\n", id, output)
	return nil
}

// appendRemainder streams from offset into the .partial file and returns the
// size on disk afterwards, whether or not the transfer completed.
func (a *app) appendRemainder(ctx context.Context, c *api.Client, id, output string, offset int64) (int64, error) {
	resp, err := c.DownloadRange(ctx, id, "", rangeHeader(offset))
	if err != nil {
		if offset > 0 && offsetPastEnd(err) {
			// The server has less content than the saved offset claims, so the
			// partial cannot belong to what is being served now.
			fmt.Fprintf(a.stderr, "warning: server rejected the resume offset; starting again\n")
			discardPartial(output)
			return a.appendRemainder(ctx, c, id, output, 0)
		}
		return offset, err
	}
	defer resp.Body.Close()

	flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
	if offset > 0 && resp.StatusCode != partialContentStatus {
		// The range was ignored and the body is the whole file. Appending it
		// would duplicate the prefix and produce a file that is the right shape
		// and the wrong content, so start the file again.
		fmt.Fprintf(a.stderr, "warning: server ignored the resume range; downloading from the start\n")
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		offset = 0
	}
	f, err := os.OpenFile(partialPath(output), flags, 0o600)
	if err != nil {
		return offset, &codedError{code: "io", err: err, exit: ExitGeneric}
	}
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return offset + n, &codedError{code: "io", err: copyErr, exit: ExitGeneric}
	}
	if closeErr != nil {
		return offset + n, &codedError{code: "io", err: closeErr, exit: ExitGeneric}
	}
	return offset + n, nil
}

func hashFileOnDisk(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

const partialContentStatus = 206

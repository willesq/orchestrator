package spaces

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
)

// Binary files and non-commit objects a commit or push sends (SP-044).
// git's diff shows a binary file as "Binary files differ"; reading every
// byte (--text) made a large image fail the check's output bound, so the
// diff stays text-only and binary files are read here, each up to
// maxBinaryRead and at most maxBinaryFiles of them. Larger or further
// binary files are not read: like any encoded content (compressed, base64,
// UTF-16), they are accepted risk under D-60.

const (
	maxBinaryRead  = 1 << 20
	maxBinaryFiles = 64
)

// binaryPaths reads the binary files of a --numstat listing, by commit
// when lines "\x01<commit>" precede them ("" for none). A path git quotes
// is unquoted; one that cannot be is an error.
func binaryPaths(numstat string) (commits, paths []string, err error) {
	commit := ""
	for _, line := range strings.Split(numstat, "\n") {
		if rest, ok := strings.CutPrefix(line, "\x01"); ok {
			commit = strings.TrimSpace(rest)
			continue
		}
		p, ok := strings.CutPrefix(line, "-\t-\t")
		if !ok {
			continue
		}
		if strings.HasPrefix(p, `"`) {
			if p, err = strconv.Unquote(p); err != nil {
				return nil, nil, errors.New("a binary file's name cannot be read")
			}
		}
		commits, paths = append(commits, commit), append(paths, p)
	}
	return commits, paths, nil
}

// blobTexts reads the binary files git lists with numstatArgs: from the
// commit each is listed under, the index (index set), or the work tree in
// dir.
func (c OutboundChecker) blobTexts(ctx context.Context, dir string, index bool, numstatArgs ...string) ([]string, error) {
	out, err := c.git(ctx, dir, numstatArgs...)
	if err != nil {
		return nil, err
	}
	commits, paths, err := binaryPaths(string(out))
	if err != nil {
		return nil, err
	}
	var texts []string
	for i, p := range paths {
		if i == maxBinaryFiles || ctx.Err() != nil {
			break
		}
		if commits[i] == "" && !index {
			if b, err := readSmall(filepath.Join(dir, p), maxBinaryRead+1); err == nil && len(b) <= maxBinaryRead {
				texts = append(texts, string(b))
			}
			continue
		}
		spec := commits[i] + ":" + p
		size, err := c.git(ctx, dir, "cat-file", "-s", spec)
		if err != nil {
			// Deleted in that commit, or a submodule: nothing to read.
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(string(size))); err != nil || n > maxBinaryRead {
			continue
		}
		b, err := c.git(ctx, dir, "cat-file", "blob", spec)
		if err != nil {
			return texts, err
		}
		texts = append(texts, string(b))
	}
	return texts, nil
}

// commitsOnly fails when a ref a push sends, or a tag it matches, is not
// a commit once peeled (a tag of a blob or a tree): the log the check
// reads shows nothing of those.
func (c OutboundChecker) commitsOnly(ctx context.Context, dir string, refs, tags []string, pending bool) error {
	for _, ref := range refs {
		if strings.HasPrefix(ref, "-") {
			continue
		}
		out, err := c.git(ctx, dir, "cat-file", "-t", ref+"^{}")
		if err != nil {
			if pending {
				// Made earlier in the same call; pending reads its content.
				continue
			}
			return err
		}
		if strings.TrimSpace(string(out)) != "commit" {
			return errors.New("a pushed ref is not a commit")
		}
	}
	var patterns []string
	for _, t := range tags {
		if t == "*" {
			patterns = append(patterns, "refs/tags")
		} else {
			patterns = append(patterns, "refs/tags/"+t)
		}
	}
	if len(patterns) == 0 {
		return nil
	}
	out, err := c.git(ctx, dir, append([]string{"for-each-ref", "--format=%(objecttype) %(*objecttype)"}, patterns...)...)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if t := f[len(f)-1]; t != "commit" {
			return errors.New("a pushed tag is not of a commit")
		}
	}
	return nil
}

package executor

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

type Target struct {
	SchemaVersion int    `json:"schema_version"`
	Revision      string `json:"revision"`
	Ref           string `json:"ref"`
	SHA256        string `json:"sha256"`
}

func (t Target) Verify(archive string) error {
	if t.SchemaVersion != 1 || !operations.FullSHA.MatchString(t.Revision) ||
		operations.ValidateRef(t.Ref) != nil ||
		!operations.SHA256.MatchString(t.SHA256) {
		return fmt.Errorf("invalid target metadata")
	}
	sum, err := operations.Digest(archive)
	if err != nil {
		return err
	}
	if sum != t.SHA256 {
		return fmt.Errorf("source artifact checksum mismatch")
	}
	return nil
}

func Resolve(ctx context.Context, request operations.Request, c *JobClient, dir string) (Target, error) {
	var target Target
	if err := request.Validate(); err != nil {
		return target, err
	}
	if _, err := capture(
		ctx,
		"git",
		"check-ref-format",
		"refs/heads/"+request.Ref,
	); err != nil &&
		!operations.FullSHA.MatchString(request.Ref) {
		return target, fmt.Errorf("invalid source ref")
	}
	if _, err := capture(ctx, "git", "fetch", "--no-tags", "origin", request.Ref); err != nil {
		return target, err
	}
	sha, err := capture(ctx, "git", "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return target, err
	}
	if !operations.FullSHA.MatchString(sha) {
		return target, fmt.Errorf("invalid source revision")
	}
	if request.Environment == "production" {
		if err = c.ProtectedTag(ctx, request.Ref, sha); err != nil {
			return target, err
		}
		if _, err = capture(
			ctx,
			"git",
			"fetch",
			"--no-tags",
			"origin",
			"+refs/heads/main:refs/remotes/origin/main",
		); err != nil {
			return target, err
		}
		if _, err = capture(
			ctx,
			"git",
			"merge-base",
			"--is-ancestor",
			sha,
			"refs/remotes/origin/main",
		); err != nil {
			return target, fmt.Errorf("production revision is not ancestral to main")
		}
	}
	repository, err := capture(ctx, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return target, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return target, err
	}
	archive := filepath.Join(dir, "source.tar")
	f, err := os.OpenFile(archive, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return target, err
	}
	arguments, err := sourceArchiveArguments(ctx, repository, sha)
	if err == nil {
		err = RunProcess(ctx, Command{Program: "git", Args: arguments, Stdout: f})
	}
	closeErr := f.Close()
	if err != nil {
		return target, err
	}
	if closeErr != nil {
		return target, closeErr
	}
	sum, err := operations.Digest(archive)
	if err != nil {
		return target, err
	}
	target = Target{SchemaVersion: 1, Revision: sha, Ref: request.Ref, SHA256: sum}
	return target, operations.WriteJSON(filepath.Join(dir, "revision.json"), target)
}

// contractExportScript is the only file the tested source keeps from scripts/: the
// backend suite checks that it exists and is executable. No job and no host runs it,
// and the application images copy only their own source directories.
const contractExportScript = "scripts/export-openapi.sh"

// sourceArchiveArguments archives the application from the requested revision.
// Pathspec exclusions always win, so scripts/ is excluded file by file around the
// export script. A revision without it keeps the whole directory excluded.
func sourceArchiveArguments(ctx context.Context, repository, sha string) ([]string, error) {
	listing, err := capture(
		ctx,
		"git",
		"-C", repository, "ls-tree", "-r", "-z", "--name-only", sha, "--", "scripts",
	)
	if err != nil {
		return nil, err
	}
	scripts := []string{":(exclude)scripts"}
	if names := strings.Split(listing, "\x00"); slices.Contains(names, contractExportScript) {
		scripts = scripts[:0]
		for _, name := range names {
			if name != "" && name != contractExportScript {
				scripts = append(scripts, ":(exclude,literal)"+name)
			}
		}
	}
	arguments := []string{
		"-C", repository, "archive", "--format=tar", sha, "--", ".",
		":(exclude)android", ":(exclude).gitlab-ci.yml",
		":(exclude).gitlab", ":(exclude).env", ":(exclude).idea",
	}
	arguments = append(arguments, scripts...)
	for _, excluded := range operations.ApplicationSourceExclusions() {
		arguments = append(arguments, ":(exclude)"+excluded)
	}
	return arguments, nil
}

// GitLab's merge_base endpoint does not accept JOB-TOKEN. Git uses an ephemeral
// credential helper; only that git process inherits the helper's credential.
func (c *JobClient) VerifyProduction(ctx context.Context, ref, sha string) error {
	if err := c.ProtectedTag(ctx, ref, sha); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)+$`).MatchString(c.settings.ProjectPath) {
		return fmt.Errorf("invalid CI project path")
	}
	for _, part := range strings.Split(c.settings.ProjectPath, "/") {
		if part == "." || part == ".." {
			return fmt.Errorf("invalid project path")
		}
	}
	base, err := url.Parse(strings.TrimSuffix(c.settings.APIURL, "/api/v4") + "/" + c.settings.ProjectPath + ".git")
	if err != nil {
		return fmt.Errorf("invalid repository URL")
	}
	tmp, err := os.MkdirTemp("", "chamados-ancestry-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	helper := `!f() { printf '%s\n' username=gitlab-ci-token; printf 'password=%s\n' "$CHAMADOS_GIT_TOKEN"; }; f`
	run := func(args ...string) (string, error) {
		b := &boundedOutput{limit: 1 << 20}
		err := RunProcess(
			ctx,
			Command{
				Program: "git",
				Args: append(
					[]string{
						"-C",
						tmp,
						"-c",
						"credential.helper=",
						"-c",
						"credential.helper=" + helper,
					},
					args...),
				Env: []string{
					"CHAMADOS_GIT_TOKEN=" + c.settings.Token,
					"GIT_TERMINAL_PROMPT=0",
					"GIT_CONFIG_GLOBAL=/dev/null",
					"GIT_CONFIG_SYSTEM=/dev/null",
				},
				Stdout: b,
			},
		)
		return strings.TrimSpace(b.String()), err
	}
	if _, err = run("init", "--bare", "--quiet"); err != nil {
		return err
	}
	if _, err = run(
		"fetch",
		"--quiet",
		"--no-tags",
		"--filter=blob:none",
		base.String(),
		"+refs/heads/main:refs/heads/main",
		"+refs/tags/"+ref+":refs/tags/"+ref,
	); err != nil {
		return err
	}
	actual, err := run("rev-parse", "refs/tags/"+ref+"^{commit}")
	if err != nil || actual != sha {
		return fmt.Errorf("production tag changed during verification")
	}
	if _, err = run("merge-base", "--is-ancestor", sha, "refs/heads/main"); err != nil {
		return fmt.Errorf("production revision is not ancestral to main")
	}
	return nil
}

package builder

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AdarshGT33/onvice/internal/queue"
	"github.com/AdarshGT33/onvice/internal/store"
	"github.com/google/uuid"
)

type Store interface {
	StartDeploymentBuild(context.Context, string) error
	GetBuildInput(context.Context, string) (store.BuildInput, error)
	PinCommitSHA(context.Context, string, string) (string, error)
	SaveBuildResult(context.Context, string, string) error
	SaveBuildFailure(context.Context, string, string, string, string) error
}

type Builder struct {
	db      Store
	baseDir string
}

func New(db Store, baseDir string) *Builder {
	if baseDir == "" {
		baseDir = filepath.Join(os.TempDir(), "onvice-builds")
	}
	return &Builder{db: db, baseDir: baseDir}
}

func (b *Builder) Handle(ctx context.Context, job store.Job) error {
	var logs strings.Builder
	id := job.DeploymentID

	// Step 1: mark the deployment as building.
	if err := b.db.StartDeploymentBuild(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotBuildable) {
			// FIX 2: the deployment has moved on; retrying can't change that.
			return queue.Permanent(err)
		}
		return fmt.Errorf("start build: %w", err)
	}

	input, err := b.db.GetBuildInput(ctx, id)
	if err != nil {
		return b.fail(ctx, id, "load_deployment", err, logs.String())
	}

	repoURL, err := normalizeGitHubURL(input.RepoURL)
	if err != nil {
		return b.fail(ctx, id, "validate_repo", queue.Permanent(err), logs.String())
	}

	// Step 2: resolve the commit only on the first attempt.
	// Retries reuse the SHA saved earlier.
	sha := input.CommitSHA
	if sha == "" {
		sha, err = resolveCommit(ctx, repoURL, input.Branch, &logs)
		if err != nil {
			return b.fail(ctx, id, "resolve_commit", err, logs.String())
		}
	}

	if !isGitSHA(sha) {
		return b.fail(ctx, id, "resolve_commit",
			queue.Permanent(fmt.Errorf("invalid commit SHA %q", sha)), logs.String())
	}

	savedSHA, err := b.db.PinCommitSHA(ctx, id, sha)
	if err != nil {
		return b.fail(ctx, id, "pin_commit", err, logs.String())
	}
	sha = savedSHA
	logs.WriteString("Pinned commit: " + sha + "\n")

	// Validate the deployment ID before using it in a folder path.
	if _, err := uuid.Parse(input.DeploymentID); err != nil {
		return b.fail(ctx, id, "prepare_directory",
			queue.Permanent(errors.New("invalid deployment UUID")), logs.String())
	}

	workDir := filepath.Join(b.baseDir, input.DeploymentID)

	if err := os.MkdirAll(b.baseDir, 0o700); err != nil {
		return b.fail(ctx, id, "prepare_directory", err, logs.String())
	}
	// Remove files left by an earlier attempt.
	if err := os.RemoveAll(workDir); err != nil {
		return b.fail(ctx, id, "prepare_directory", err, logs.String())
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return b.fail(ctx, id, "prepare_directory", err, logs.String())
	}
	// Clean up whether the clone succeeds or fails.
	defer func() {
		if err := os.RemoveAll(workDir); err != nil {
			fmt.Fprintf(os.Stderr, "cleanup %s: %v\n", workDir, err)
		}
	}()

	// Steps 3 and 4: fetch exactly the pinned commit, not the branch tip.
	steps := [][]string{
		{"init"},
		{"remote", "add", "origin", repoURL},
		{"fetch", "--depth", "1", "origin", sha},
		{"checkout", "--detach", "FETCH_HEAD"},
	}
	for _, args := range steps {
		if _, err := runGit(ctx, workDir, &logs, args...); err != nil {
			return b.fail(ctx, id, "clone", err, logs.String())
		}
	}

	// Record the files that arrived.
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return b.fail(ctx, id, "inspect_checkout", err, logs.String())
	}
	logs.WriteString("Repository root files:\n")
	for _, entry := range entries {
		logs.WriteString("  " + entry.Name() + "\n")
	}

	// Save the log with a fresh context, so it's saved even during shutdown.
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.db.SaveBuildResult(saveCtx, id, logs.String()); err != nil {
		return fmt.Errorf("save build log: %w", err)
	}
	return nil
}

// FIX 1: resolve the project's branch, not always the default branch.
// An empty branch falls back to HEAD, the repository's default branch.
func resolveCommit(ctx context.Context, repoURL, branch string, logs *strings.Builder) (string, error) {
	ref := "HEAD"
	if branch != "" {
		ref = "refs/heads/" + branch
	}

	output, err := runGit(ctx, "", logs, "ls-remote", repoURL, ref)
	if err != nil {
		return "", err
	}

	sha, ok := findRef(output, ref)
	if !ok {
		if branch == "" {
			return "", queue.Permanent(errors.New("the repository is empty"))
		}
		return "", queue.Permanent(fmt.Errorf("branch %q doesn't exist in the repository", branch))
	}
	return sha, nil
}

// findRef picks the line whose ref matches exactly. ls-remote matches
// patterns loosely, so it can return more than one line.
func findRef(output, ref string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == ref {
			return fields[0], true
		}
	}
	return "", false
}

func runGit(parent context.Context, dir string, logs *strings.Builder, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()

	// FIX 4: "-c credential.helper=" turns off credential helpers, so
	// Git Credential Manager can't open a sign-in window on Windows.
	fullArgs := append([]string{"-c", "credential.helper="}, args...)

	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // never wait for a username in the terminal
		"GCM_INTERACTIVE=never", // and never in a window
	)

	output, err := cmd.CombinedOutput()

	logs.WriteString("$ git " + strings.Join(args, " ") + "\n")
	logs.Write(output)
	if len(output) == 0 || output[len(output)-1] != '\n' {
		logs.WriteByte('\n')
	}

	if err != nil {
		// FIX 3: if the job itself was stopped, say so instead of
		// reporting a git failure.
		switch {
		case errors.Is(parent.Err(), context.Canceled):
			// Shutdown: the worker hands the job back.
			return "", parent.Err()
		case parent.Err() != nil:
			// The job's 10-minute limit ran out: a real failure, worth retrying.
			return "", fmt.Errorf("git %s: job time limit reached: %w", args[0], parent.Err())
		}
		err = fmt.Errorf("git %s failed: %w", args[0], err)
		// FIX 2: some git failures will never succeed on retry.
		if isPermanentGitFailure(string(output)) {
			err = queue.Permanent(err)
		}
		return string(output), err
	}
	return string(output), nil
}

// isPermanentGitFailure recognizes errors that retrying can't fix.
// Anything else (network trouble, timeouts) is worth retrying.
func isPermanentGitFailure(output string) bool {
	out := strings.ToLower(output)
	for _, s := range []string{
		"repository not found",      // deleted or renamed
		"terminal prompts disabled", // repo became private
		"authentication failed",     // repo became private
		"couldn't find remote ref",  // branch or commit is gone
		"not our ref",               // commit no longer exists
	} {
		if strings.Contains(out, s) {
			return true
		}
	}
	return false
}

func (b *Builder) fail(ctx context.Context, deploymentID, step string, cause error, logText string) error {
	// FIX 3: a shutdown isn't a build failure, so record nothing; the worker
	// hands the job back. Only Canceled means shutdown: DeadlineExceeded
	// means the job hit its time limit, which IS a failure worth recording.
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("%s interrupted: %w", step, ctx.Err())
	}

	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if saveErr := b.db.SaveBuildFailure(saveCtx, deploymentID, step, cause.Error(), logText); saveErr != nil {
		return fmt.Errorf("%s failed: %w; also failed to save failure details: %v", step, cause, saveErr)
	}
	// %w keeps queue.ErrPermanent visible to the worker.
	return fmt.Errorf("%s failed: %w", step, cause)
}

func normalizeGitHubURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid repository URL")
	}
	if u.Scheme != "https" ||
		!strings.EqualFold(u.Hostname(), "github.com") ||
		u.Port() != "" ||
		u.User != nil ||
		u.RawQuery != "" ||
		u.Fragment != "" {
		return "", errors.New("only HTTPS GitHub repository URLs are supported")
	}

	path := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 ||
		parts[0] == "" || parts[1] == "" ||
		parts[0] == "." || parts[0] == ".." ||
		parts[1] == "." || parts[1] == ".." {
		return "", errors.New("expected a GitHub URL in owner/repository format")
	}
	return "https://github.com/" + parts[0] + "/" + parts[1] + ".git", nil
}

func isGitSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, ch := range sha {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

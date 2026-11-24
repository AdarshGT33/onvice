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
	MarkDeploymentBuilt(context.Context, string, string, int, string) error
	SaveFailure(context.Context, string, string, string, string) error
	StartDeploymentDeploy(context.Context, string) (store.DeployInput, error)
	MarkDeploymentHealthy(context.Context, string, int, string) error
	KeepImageTags(context.Context, string) ([]string, error)
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

	// Step 5: build the image. .git is history, not the app: dropping it
	// keeps it out of the image, and out of what a static site serves.
	if err := os.RemoveAll(filepath.Join(workDir, ".git")); err != nil {
		return b.fail(ctx, id, "prepare_build", err, logs.String())
	}
	generated, err := prepareDockerfile(workDir)
	if err != nil {
		return b.fail(ctx, id, "detect_app", err, logs.String())
	}
	if generated == "" {
		logs.WriteString("Using the repository's own Dockerfile\n")
	} else {
		logs.WriteString("Generated Dockerfile:\n" + generated)
	}

	// One image name per app, one tag per commit. A rebuild of the same
	// commit moves the tag to the new image and leaves the old one untagged
	// ("dangling"); the label below is how cleanup still finds it.
	imageTag := strings.ToLower("onvice/" + input.AppID + ":" + sha[:12])

	// No extra timeout: the build may use whatever is left of the job's
	// 10 minutes. A failure here is retried, because a registry or network
	// hiccup looks the same as broken code from outside.
	if _, err := run(ctx, 0, workDir, &logs, nil,
		"docker", "build", "--progress=plain", "-t", imageTag,
		// NEW: marks the image as this app's, even after it loses its tag.
		"--label", "onvice.app="+input.AppID,
		"."); err != nil {
		return b.fail(ctx, id, "build_image", err, logs.String())
	}

	// Step 6: the deployer needs to know which port to route traffic to.
	inspect, err := run(ctx, time.Minute, "", &logs, nil,
		"docker", "image", "inspect", "--format", "{{json .Config.ExposedPorts}}", imageTag)
	if err != nil {
		return b.fail(ctx, id, "inspect_image", err, logs.String())
	}
	port, err := exposedPort(inspect)
	if err != nil {
		return b.fail(ctx, id, "inspect_image", err, logs.String())
	}
	logs.WriteString(fmt.Sprintf("Built image %s, port %d\n", imageTag, port))

	// Save with a fresh context, so the result is saved even during shutdown.
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.db.MarkDeploymentBuilt(saveCtx, id, imageTag, port, logs.String()); err != nil {
		return fmt.Errorf("save build result: %w", err)
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

// FIX 4: the GIT_CONFIG_* trio is the same as "-c credential.helper=":
// it turns off credential helpers, so Git Credential Manager can't open a
// sign-in window on Windows. Env instead of a flag keeps args[0] the git
// subcommand, which error messages and the build log show.
var gitEnv = []string{
	"GIT_TERMINAL_PROMPT=0", // never wait for a username in the terminal
	"GCM_INTERACTIVE=never", // and never in a window
	"GIT_CONFIG_COUNT=1",
	"GIT_CONFIG_KEY_0=credential.helper",
	"GIT_CONFIG_VALUE_0=",
}

func runGit(ctx context.Context, dir string, logs *strings.Builder, args ...string) (string, error) {
	output, err := run(ctx, 2*time.Minute, dir, logs, gitEnv, "git", args...)
	// FIX 2: some git failures will never succeed on retry.
	// Skipped when the job itself was stopped: that isn't git's fault.
	if err != nil && ctx.Err() == nil && isPermanentGitFailure(output) {
		err = queue.Permanent(err)
	}
	return output, err
}

// run executes one command and appends it, with its output, to logs.
// CHANGED: shared by git and docker. timeout 0 means only the job's own
// deadline applies.
func run(parent context.Context, timeout time.Duration, dir string, logs *strings.Builder,
	env []string, name string, args ...string) (string, error) {
	ctx := parent
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parent, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir // "" means the current directory
	cmd.Env = append(os.Environ(), env...)

	output, err := cmd.CombinedOutput()

	logs.WriteString("$ " + name + " " + strings.Join(args, " ") + "\n")
	logs.Write(output)
	if len(output) == 0 || output[len(output)-1] != '\n' {
		logs.WriteByte('\n')
	}

	if err != nil {
		// FIX 3: if the job itself was stopped, say so instead of
		// reporting a command failure.
		switch {
		case errors.Is(parent.Err(), context.Canceled):
			// Shutdown: the worker hands the job back.
			return "", parent.Err()
		case parent.Err() != nil:
			// The job's 10-minute limit ran out: a real failure, worth retrying.
			return "", fmt.Errorf("%s %s: job time limit reached: %w", name, args[0], parent.Err())
		}
		return string(output), fmt.Errorf("%s %s failed: %w", name, args[0], err)
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

	// CHANGED: end the log with the reason. Without it, the log a user reads
	// stops at the last command that ran, and the cause is only in
	// error_message, a different field.
	logText += fmt.Sprintf("FAILED at %s: %v\n", step, cause)

	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if saveErr := b.db.SaveFailure(saveCtx, deploymentID, step, cause.Error(), logText); saveErr != nil {
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

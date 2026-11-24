package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AdarshGT33/onvice/internal/auth"
	"github.com/AdarshGT33/onvice/internal/builder"
	"github.com/AdarshGT33/onvice/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// appName is a valid DNS label (RFC 1123), since the name becomes
// <name>.<APPS_DOMAIN>. Anything else could never be routed to.
var appName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type InputType struct {
	GithubURL   string `json:"github_url" binding:"required"`
	ProjectName string `json:"name" binding:"required"`
}

var (
	ErrRepoNotFound      = errors.New("repository does not exist or is private")
	ErrRepoPrivate       = errors.New("private repositories aren't supported yet; use a public repository")
	ErrGitHubUnavailable = errors.New("couldn't reach GitHub right now, please try again shortly")
)

type RateLimitError struct {
	RetryAfter time.Duration
}

type Handler struct {
	store *store.Store
	// CHANGED: appURL is the public URL pattern, e.g.
	// "http://{name}.localhost:8000", so responses can tell users where
	// their app lives. Comes from config: behind TLS or a load balancer the
	// public URL differs from what the router listens on.
	appURL string
}

func NewHandler(s *store.Store, appURL string) *Handler {
	return &Handler{store: s, appURL: appURL}
}

// url is the app's stable public URL. It is known from the moment the
// app is created, before anything is deployed: it never changes.
func (h *Handler) url(name string) string {
	return strings.ReplaceAll(h.appURL, "{name}", name)
}

// ListProjects returns the caller's apps with their URL, whether they are
// live, and what their newest deployment is doing.
// NEW: GET /api/v1/projects
func (h *Handler) ListProjects(c *gin.Context) {
	apps, err := h.store.ListApps(c.Request.Context(), auth.UserID(c))
	if err != nil {
		_ = c.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list projects"})
		return
	}

	// Always a JSON array, never null, even with no projects: clients can
	// loop over it without a special case.
	projects := make([]gin.H, 0, len(apps))
	for _, a := range apps {
		p := gin.H{
			"app_id":     a.ID,
			"name":       a.Name,
			"github_url": a.RepoURL,
			"url":        h.url(a.Name),
			"live":       a.Live,
			"created_at": a.CreatedAt,
		}
		if a.LatestDeploymentID != nil {
			p["latest_deployment"] = gin.H{
				"deployment_id": *a.LatestDeploymentID,
				"status":        a.LatestStatus,
				"error_message": a.LatestErrorMessage,
				"created_at":    a.LatestCreatedAt,
			}
		}
		projects = append(projects, p)
	}
	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("GitHub rate limit reached, try again in %s", e.RetryAfter.Round(time.Second))
}

func (h *Handler) TakingInputofRepoandProjectName(c *gin.Context) {
	var input InputType

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": " Project Name and GitHub URL are required",
		})
		return
	}

	// CHANGED: lowercased, because the name becomes the app's subdomain and
	// hostnames are case-insensitive: "MyApp" and "myapp" would be one URL.
	input.ProjectName = strings.ToLower(strings.TrimSpace(input.ProjectName))
	input.GithubURL = strings.TrimSpace(input.GithubURL)

	if !appName.MatchString(input.ProjectName) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Project name becomes your app's subdomain: use 1-63 letters, digits or hyphens, starting and ending with a letter or digit",
		})
		return
	}

	if err := ValidateGitHubURL(input.GithubURL); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := VerifyGithubURL(c.Request.Context(), input.GithubURL); err != nil {
		var rateLimited *RateLimitError
		switch {
		case errors.As(err, &rateLimited):
			// 503: our server can't serve this right now; the request itself was fine.
			seconds := int((rateLimited.RetryAfter + time.Second - 1) / time.Second)
			c.Header("Retry-After", strconv.Itoa(seconds))
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": rateLimited.Error()})
		case errors.Is(err, ErrRepoNotFound), errors.Is(err, ErrRepoPrivate):
			// 422: the request is well formed, but points at a repo we can't use.
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		default:
			// 502: GitHub, not the client, failed. Keep the detail for our logs only.
			_ = c.Error(err)
			c.JSON(http.StatusBadGateway, gin.H{"error": ErrGitHubUnavailable.Error()})
		}
		return
	}

	app, deployment, err := h.store.CreateAppWithDeployment(
		c.Request.Context(),
		auth.UserID(c),
		input.ProjectName,
		input.GithubURL,
	)
	if err != nil {
		if errors.Is(err, store.ErrNameTaken) {
			c.JSON(http.StatusConflict, gin.H{
				"error": "A project with this name already exists",
			})
			return
		}

		_ = c.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to create deployment",
		})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"message":       "Deployment queued",
		"app_id":        app.ID,
		"name":          app.Name,
		"github_url":    app.RepoURL,
		"url":           h.url(app.Name),
		"deployment_id": deployment.ID,
		"status":        deployment.Status,
	})

}

// Redeploy builds the app's latest commit and swaps it in with no
// downtime: the current version keeps serving until the new one is healthy.
// It is also how a user recovers after a failed deploy: push a fix, redeploy.
// NEW: POST /api/v1/projects/:name/deployments
func (h *Handler) Redeploy(c *gin.Context) {
	// Lowercase, like at creation: the name is a hostname.
	name := strings.ToLower(c.Param("name"))

	deployment, err := h.store.CreateDeployment(c.Request.Context(), auth.UserID(c), name)
	switch {
	case errors.Is(err, store.ErrAppNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "No project named " + name})
		return
	case errors.Is(err, store.ErrDeploymentInProgress):
		// 409: the app is mid-deploy. Point at that deployment so the
		// client can follow it and redeploy once it settles.
		body := gin.H{"error": "A deployment of this project is already in progress; wait for it to finish, then redeploy"}
		if deployment.ID != "" {
			body["deployment_id"] = deployment.ID
			body["status"] = deployment.Status
		}
		c.JSON(http.StatusConflict, body)
		return
	case err != nil:
		_ = c.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create deployment"})
		return
	}

	// 202: accepted, the work happens in the background. Same shape as
	// POST /projects, so clients poll GET /deployments/:id either way.
	c.JSON(http.StatusAccepted, gin.H{
		"message":       "Deployment queued",
		"app_id":        deployment.AppID,
		"name":          name,
		"url":           h.url(name),
		"deployment_id": deployment.ID,
		"status":        deployment.Status,
	})
}

// Rollback puts the app's previous successful version back live, reusing
// its image: no clone, no build, so it takes seconds, not minutes, which
// matters most exactly when a bad deploy is hurting users. Calling it
// again rolls forward to the version just replaced.
// NEW: POST /api/v1/projects/:name/rollback
func (h *Handler) Rollback(c *gin.Context) {
	name := strings.ToLower(c.Param("name"))

	deployment, err := h.store.CreateRollback(c.Request.Context(), auth.UserID(c), name)
	switch {
	case errors.Is(err, store.ErrAppNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "No project named " + name})
		return
	case errors.Is(err, store.ErrNoRollbackTarget):
		c.JSON(http.StatusConflict, gin.H{"error": "This project has no previous successful version to roll back to"})
		return
	case errors.Is(err, store.ErrDeploymentInProgress):
		// Same rule as redeploy: one deployment in flight per app.
		body := gin.H{"error": "A deployment of this project is already in progress; wait for it to finish, then roll back"}
		if deployment.ID != "" {
			body["deployment_id"] = deployment.ID
			body["status"] = deployment.Status
		}
		c.JSON(http.StatusConflict, body)
		return
	case err != nil:
		_ = c.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start rollback"})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"message":       "Rollback queued",
		"app_id":        deployment.AppID,
		"name":          name,
		"url":           h.url(name),
		"deployment_id": deployment.ID,
		"status":        deployment.Status,
		// Which version is coming back, so the user can confirm it's the right one.
		"commit_sha": deployment.CommitSHA,
		"image_tag":  deployment.ImageTag,
	})
}

// DeploymentLogs returns a deployment's build and deploy log: every git,
// docker build and health-check step with its output, so users can see
// why a deploy failed, not just the one-line error_message.
// NEW: GET /api/v1/deployments/:id/logs
func (h *Handler) DeploymentLogs(c *gin.Context) {
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid deployment ID"})
		return
	}

	logText, err := h.store.DeploymentLog(c.Request.Context(), auth.UserID(c), id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		c.JSON(http.StatusNotFound, gin.H{"error": "Deployment not found"})
		return
	case err != nil:
		_ = c.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read deployment log"})
		return
	}
	plainText(c, logText)
}

// AppLogs returns the last lines the app's live container printed: what
// the app itself is doing (crashes, errors, its own request logs).
// NEW: GET /api/v1/projects/:name/logs?lines=200
func (h *Handler) AppLogs(c *gin.Context) {
	name := strings.ToLower(c.Param("name"))

	// Bounded: a huge tail would make Docker read and us send megabytes
	// for one request.
	lines := 200
	if s := c.Query("lines"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 5000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "lines must be a number from 1 to 5000"})
			return
		}
		lines = n
	}

	depID, err := h.store.LiveDeploymentID(c.Request.Context(), auth.UserID(c), name)
	switch {
	case errors.Is(err, store.ErrAppNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "No project named " + name})
		return
	case errors.Is(err, store.ErrAppNotLive):
		// 409: the app exists but isn't in a state that has runtime logs.
		c.JSON(http.StatusConflict, gin.H{
			"error": "This project isn't running, so it has no runtime logs; see its latest deployment's logs at GET /api/v1/deployments/:id/logs",
		})
		return
	case err != nil:
		_ = c.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read logs"})
		return
	}

	out, err := builder.ContainerLogs(c.Request.Context(), depID, lines)
	if err != nil {
		_ = c.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read logs"})
		return
	}
	plainText(c, out)
}

// plainText sends logs as text/plain, not JSON: they read directly in a
// terminal (curl) and pipe into grep. nosniff stops a browser from ever
// treating log text that happens to look like HTML as a page.
func plainText(c *gin.Context, body string) {
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(body))
}

func (h *Handler) GetDeployment(c *gin.Context) {
	id := c.Param("id")

	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Invalid deployment ID",
		})
		return
	}

	deployment, err := h.store.GetDeploymentByID(
		c.Request.Context(),
		auth.UserID(c),
		id,
	)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": "Deployment not found",
			})
			return
		}

		_ = c.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to retrieve deployment",
		})
		return
	}

	c.JSON(http.StatusOK, deployment)
}

func ValidateGitHubURL(rawURL string) error {
	u, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return errors.New("invalid URL format")
	}

	if u.User != nil {
		return errors.New("remove the username or token from the URL; public repositories don't need one")
	}

	if u.Scheme != "https" || u.Host != "github.com" {
		return errors.New("URL must use https://github.com/")
	}

	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("query parameters and fragments are not allowed")
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")

	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return errors.New(
			"URL must follow https://github.com/owner/repo",
		)
	}

	return nil
}

func VerifyGithubURL(ctx context.Context, rawURL string) error {
	u, err := url.ParseRequestURI(rawURL)

	if err != nil {
		return errors.New("invalid URL format")
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("URL must follow https://github.com/owner/repo")
	}

	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")

	if repo == "" {
		return fmt.Errorf("repository name cannot be empty")
	}

	apiURL := "https://api.github.com/repos/" +
		url.PathEscape(owner) + "/" + url.PathEscape(repo)

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return fmt.Errorf("%w: failed to create GitHub request: %v", ErrGitHubUnavailable, err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "my-go-server")

	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrGitHubUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		// With a token, a private repo the token's owner can see also returns 200,
		// so check it's really public.
		var body struct {
			Private bool `json:"private"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return fmt.Errorf("%w: unreadable response: %v", ErrGitHubUnavailable, err)
		}
		if body.Private {
			return ErrRepoPrivate
		}
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return ErrRepoNotFound
	case isRateLimited(resp):
		return &RateLimitError{RetryAfter: retryAfter(resp)}
	default:
		return fmt.Errorf("%w: GitHub API returned status %d", ErrGitHubUnavailable, resp.StatusCode)
	}
}

func isRateLimited(resp *http.Response) bool {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return false
	}
	return resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""
}

// retryAfter reads how long GitHub wants us to wait, falling back to a minute.
func retryAfter(resp *http.Response) time.Duration {
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		return time.Duration(s) * time.Second
	}
	if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		if d := time.Until(time.Unix(reset, 0)); d > 0 {
			return d
		}
	}
	return time.Minute
}

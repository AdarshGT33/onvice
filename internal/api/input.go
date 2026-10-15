package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AdarshGT33/onvice/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

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
}

func NewHandler(s *store.Store) *Handler { return &Handler{store: s} }

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

	input.ProjectName = strings.TrimSpace(input.ProjectName)
	input.GithubURL = strings.TrimSpace(input.GithubURL)

	if input.ProjectName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Project Name cannot be empty",
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
		"deployment_id": deployment.ID,
		"status":        deployment.Status,
	})

}

func (h *Handler) GetDeployment(c *gin.Context) {
	id := c.Param("id")

	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Invalid deployment ID",
		})
	}

	deployment, err := h.store.GetDeploymentByID(
		c.Request.Context(),
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

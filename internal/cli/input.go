package cli

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

type InputType struct {
	GithubURL   string `json:"github_url" binding:"required"`
	ProjectName string `json:"name" binding:"required"`
}

func TakingInput(c *gin.Context) {
	var input InputType

	// Parse and validate the JSON request body.
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

	c.JSON(http.StatusOK, gin.H{
		"message":    "Repository input accepted",
		"name":       input.ProjectName,
		"github_url": input.GithubURL,
	})
}

func ValidateGitHubURL(rawURL string) error {
	u, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return errors.New("invalid URL format")
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

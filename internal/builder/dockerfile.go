package builder

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AdarshGT33/onvice/internal/queue"
)

// The platform writes the Dockerfile, so a repo doesn't need one.
// Contract for generated images, the same as Heroku and Render: the app
// listens on $PORT, which is 8080. A repo that ships its own Dockerfile
// is built with it unchanged.

const nodeDockerfile = `FROM node:22-alpine
WORKDIR /app
COPY package*.json ./
RUN %s
COPY . .
RUN npm run build --if-present
ENV NODE_ENV=production PORT=8080
EXPOSE 8080
CMD exec %s
`

const pythonDockerfile = `FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
ENV PYTHONUNBUFFERED=1 PORT=8080
EXPOSE 8080
CMD exec %s
`

const goDockerfile = `FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/app .

FROM alpine:3.22
COPY --from=build /out/app /app
ENV PORT=8080
EXPOSE 8080
CMD ["/app"]
`

const staticDockerfile = `FROM nginx:1.27-alpine
COPY . /usr/share/nginx/html
EXPOSE 80
`

// prepareDockerfile makes sure dir has a Dockerfile. It returns the text
// it wrote, or "" when the repository brings its own.
func prepareDockerfile(dir string) (string, error) {
	if exists(dir, "Dockerfile") {
		return "", nil
	}
	content, err := generateDockerfile(dir)
	if err != nil {
		// The repo's contents decide this; retrying can't change them.
		return "", queue.Permanent(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("write Dockerfile: %w", err)
	}
	return content, nil
}

// generateDockerfile picks a template from the files at the repo root.
// Order matters: a Node or Python app often has an index.html too.
func generateDockerfile(dir string) (string, error) {
	web := procfileWeb(dir)

	switch {
	case exists(dir, "package.json"):
		install := "npm install"
		if exists(dir, "package-lock.json") {
			install = "npm ci" // exact versions from the lockfile
		}
		if web == "" {
			web = "npm start" // npm falls back to "node server.js" without a start script
		}
		return fmt.Sprintf(nodeDockerfile, install, web), nil

	case exists(dir, "requirements.txt"):
		for _, f := range []string{"main.py", "app.py"} {
			if web == "" && exists(dir, f) {
				web = "python " + f
			}
		}
		if web == "" {
			return "", errors.New(`couldn't tell how to start this Python app: add a Procfile with a "web:" line, or a main.py or app.py`)
		}
		return fmt.Sprintf(pythonDockerfile, web), nil

	case exists(dir, "go.mod"):
		return goDockerfile, nil

	case exists(dir, "index.html"):
		return staticDockerfile, nil
	}

	return "", errors.New("couldn't detect the app type: expected package.json, requirements.txt, go.mod, index.html or a Dockerfile at the repository root")
}

// procfileWeb returns the command on the Procfile's "web:" line, if any.
// Heroku's convention, so many existing repos already have one.
func procfileWeb(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "Procfile"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if cmd, ok := strings.CutPrefix(strings.TrimSpace(line), "web:"); ok {
			return strings.TrimSpace(cmd)
		}
	}
	return ""
}

// exposedPort picks the port from `docker image inspect` output for
// .Config.ExposedPorts, e.g. {"8080/tcp":{}}. With several, the lowest
// TCP port wins (assumption A4).
func exposedPort(inspectJSON string) (int, error) {
	var ports map[string]struct{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(inspectJSON)), &ports); err != nil {
		return 0, fmt.Errorf("read exposed ports: %w", err)
	}
	best := 0
	for key := range ports {
		num, proto, _ := strings.Cut(key, "/")
		p, err := strconv.Atoi(num)
		if (proto != "" && proto != "tcp") || err != nil || p < 1 || p > 65535 {
			continue
		}
		if best == 0 || p < best {
			best = p
		}
	}
	if best == 0 {
		return 0, queue.Permanent(errors.New("the image doesn't EXPOSE a TCP port; add EXPOSE <port> to the Dockerfile"))
	}
	return best, nil
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

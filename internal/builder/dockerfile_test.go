package builder

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AdarshGT33/onvice/internal/queue"
)

func TestGenerateDockerfile(t *testing.T) {
	cases := []struct {
		files map[string]string
		want  string // substring of the Dockerfile; "" means an error
	}{
		{map[string]string{"package.json": "{}"}, "RUN npm install"},
		{map[string]string{"package.json": "{}", "package-lock.json": "{}"}, "RUN npm ci"},
		{map[string]string{"package.json": "{}", "index.html": ""}, "FROM node:"},
		{map[string]string{"package.json": "{}", "Procfile": "worker: x\nweb: node api.js\n"}, "CMD exec node api.js"},
		{map[string]string{"requirements.txt": "", "app.py": ""}, "CMD exec python app.py"},
		{map[string]string{"requirements.txt": "", "Procfile": "web: gunicorn a:b -b 0.0.0.0:$PORT"}, "gunicorn a:b"},
		{map[string]string{"requirements.txt": ""}, ""},
		{map[string]string{"go.mod": "module x"}, "go build"},
		{map[string]string{"index.html": ""}, "FROM nginx"},
		{map[string]string{"README.md": ""}, ""},
	}
	for _, c := range cases {
		dir := t.TempDir()
		for name, body := range c.files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got, err := prepareDockerfile(dir)
		if c.want == "" {
			if !errors.Is(err, queue.ErrPermanent) {
				t.Errorf("%v: want permanent error, got %v", c.files, err)
			}
			continue
		}
		if err != nil || !strings.Contains(got, c.want) {
			t.Errorf("%v: want %q in Dockerfile, got %q (err %v)", c.files, c.want, got, err)
		}
	}

	// A repo's own Dockerfile is used as is.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch"), 0o600)
	if got, err := prepareDockerfile(dir); got != "" || err != nil {
		t.Errorf("own Dockerfile: got %q, %v", got, err)
	}
}

func TestExposedPort(t *testing.T) {
	for in, want := range map[string]int{
		`{"8080/tcp":{}}`:               8080,
		`{"9000/tcp":{},"3000/tcp":{}}`: 3000,
		`{"53/udp":{},"80/tcp":{}}`:     80,
		"null\n":                        0,
		`{"53/udp":{}}`:                 0,
	} {
		got, err := exposedPort(in)
		if got != want || (want == 0) != errors.Is(err, queue.ErrPermanent) {
			t.Errorf("%s: got %d, %v; want %d", in, got, err, want)
		}
	}
}

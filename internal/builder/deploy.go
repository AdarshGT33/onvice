package builder

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AdarshGT33/onvice/internal/queue"
	"github.com/AdarshGT33/onvice/internal/store"
)

// How long a new container gets to answer HTTP before the deploy fails.
const healthTimeout = 60 * time.Second

// Deploy runs a built image, waits until it answers HTTP, then makes it the
// app's live deployment and removes the app's older containers.
// Start new, check, then switch: the old version keeps serving until the
// new one is proven, so an update causes no downtime.
func (b *Builder) Deploy(ctx context.Context, job store.Job) (err error) {
	var logs strings.Builder
	id := job.DeploymentID

	in, err := b.db.StartDeploymentDeploy(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotDeployable) {
			return queue.Permanent(err)
		}
		return fmt.Errorf("start deploy: %w", err)
	}

	// One name per deployment: a retry replaces its own leftover container
	// instead of starting a second copy next to it.
	name := "onvice-" + id
	removeContainers(name)

	// Any failure from here on removes the container, so a broken app
	// doesn't keep holding memory and a port.
	defer func() {
		if err != nil {
			removeContainers(name)
		}
	}()

	// CHANGED: we pick the host port and pin it, instead of letting Docker
	// pick one. Docker can hand out new random ports after a daemon or host
	// restart, and then a stored port could point at another app's
	// container: the router would send one user's traffic to someone else's
	// app. A pinned port belongs to this container for its whole life.
	hostPort, err := freePort()
	if err != nil {
		return b.fail(ctx, id, "pick_port", err, logs.String())
	}

	port := strconv.Itoa(in.Port)
	if _, err := run(ctx, 2*time.Minute, "", &logs, nil, "docker", "run", "--detach",
		"--name", name,
		// Lets the platform find every container of an app, see removeOtherContainers.
		"--label", "onvice.app="+in.AppID,
		// Restart after a crash or a host reboot, until we stop it on purpose.
		"--restart", "unless-stopped",
		// One app can't starve the host or the other apps (assumption A11).
		"--memory", "512m", "--cpus", "1",
		"--env", "PORT="+port,
		// 127.0.0.1: reachable only from this machine, i.e. through the
		// router. A separate host port per container, so two apps that
		// both listen on 8080 inside never clash outside.
		"--publish", "127.0.0.1:"+strconv.Itoa(hostPort)+":"+port,
		in.ImageTag); err != nil {
		// Includes "port is already allocated" if something grabbed the
		// port in the moment since freePort; the retry picks a new one.
		return b.fail(ctx, id, "run_container", err, logs.String())
	}

	url := "http://127.0.0.1:" + strconv.Itoa(hostPort) + "/"
	logs.WriteString("Health check: waiting up to " + healthTimeout.String() + " for " + url + "\n")
	if err := waitHealthy(ctx, url, healthTimeout); err != nil {
		if ctx.Err() == nil {
			// The app's own output usually says why it didn't come up.
			run(ctx, time.Minute, "", &logs, nil, "docker", "logs", "--tail", "50", name)
			// It had a full minute; a retry would most likely fail the same way.
			err = queue.Permanent(err)
		}
		return b.fail(ctx, id, "health_check", err, logs.String())
	}
	logs.WriteString("Healthy: " + in.ImageTag + " answers on " + url + "\n")

	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.db.MarkDeploymentHealthy(saveCtx, id, hostPort, logs.String()); err != nil {
		return fmt.Errorf("save deploy result: %w", err)
	}

	// Only now, with the new version live, remove the app's other containers.
	removeOtherContainers(in.AppID, name)
	return nil
}

// waitHealthy polls url until it answers with a status below 500.
// A 404 still counts: it proves the app is up and serving HTTP, and many
// APIs have nothing at "/". 5xx or no answer means it isn't ready.
func waitHealthy(ctx context.Context, url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}

	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return fmt.Errorf("app didn't answer HTTP within %s (last error: %v)", timeout, lastErr)
		case <-time.After(time.Second):
		}
	}
}

// freePort asks the OS for a free port on 127.0.0.1 and releases it for
// Docker to bind. Another process could take it in between; docker run
// then fails and the job's retry tries a new port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find a free port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// removeOtherContainers removes every container of the app except keep.
// Matching by label rather than by the previous deployment's ID also
// sweeps up containers that a crash left behind.
func removeOtherContainers(appID, keep string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var discard strings.Builder
	out, err := run(ctx, 0, "", &discard, nil, "docker", "ps", "--all",
		"--filter", "label=onvice.app="+appID, "--format", "{{.Names}}")
	if err != nil {
		// ponytail: a failed cleanup leaves the old container running until
		// the app's next deploy sweeps it; add a periodic sweep if that matters.
		log.Printf("list containers of app %s: %v", appID, err)
		return
	}
	var old []string
	for _, n := range strings.Fields(out) {
		if n != keep {
			old = append(old, n)
		}
	}
	if len(old) == 0 {
		return
	}
	// CHANGED: stop gracefully before removing. The router already sends new
	// requests to the new container; SIGTERM plus 10 seconds lets the old one
	// finish the requests it is still answering. A kill would cut them off.
	if _, err := run(ctx, 0, "", &discard, nil, "docker",
		append([]string{"stop", "--time", "10"}, old...)...); err != nil {
		log.Printf("stop old containers %v: %v", old, err)
	}
	removeContainers(old...)
}

// removeContainers force-removes containers, best effort. It runs on a
// fresh context so cleanup still happens during shutdown, and a container
// that is already gone is fine (docker exits 0).
func removeContainers(names ...string) {
	if len(names) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var out strings.Builder
	if _, err := run(ctx, 0, "", &out, nil, "docker", append([]string{"rm", "--force"}, names...)...); err != nil {
		log.Printf("remove containers %v: %v\n%s", names, err, out.String())
	}
}

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// SSHTunnel opens a local TCP forward through a remote SSH host.
//
// For `--via user@host --upstream remote-pg:5432`, pgloupe:
//  1. Picks a free localhost port.
//  2. Execs `ssh -N -L freeport:remote-pg:5432 user@host` as a subprocess.
//  3. Polls until the local end accepts a TCP connection.
//  4. Returns localhost:freeport as the address pgloupe.Serve forwards to.
//
// For `--via user@host --container name`, pgloupe additionally:
//  0. Execs `ssh user@host docker inspect ...` to resolve the container's
//     bridge IP on the configured Docker network (default "private").
//
// Reuses the user's ~/.ssh/config, ssh-agent, known_hosts, ProxyJump etc.
// because we shell out to the system `ssh` binary instead of implementing
// the protocol ourselves. This is a lot of mileage for ~150 LOC.
type SSHTunnel struct {
	cmd       *exec.Cmd
	LocalAddr string
}

// SSHTunnelConfig describes how to open the tunnel.
type SSHTunnelConfig struct {
	Via              string // user@host[:port], required
	Upstream         string // remote PG address as seen from the SSH host
	Container        string // optional: resolve via `docker inspect` on the SSH host
	DockerNetwork    string // optional: docker network name (default "private")
	RemotePort       int    // port to forward to (default 5432)
	ConnectTimeout   time.Duration
	ReadyPollTimeout time.Duration
}

// OpenSSHTunnel sets up the tunnel, blocks until it's ready, and returns
// a handle the caller must Close() when done.
func OpenSSHTunnel(ctx context.Context, cfg SSHTunnelConfig) (*SSHTunnel, error) {
	if cfg.Via == "" {
		return nil, errors.New("--via is required")
	}
	if cfg.RemotePort == 0 {
		cfg.RemotePort = 5432
	}
	if cfg.DockerNetwork == "" {
		cfg.DockerNetwork = "private"
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 15 * time.Second
	}
	if cfg.ReadyPollTimeout == 0 {
		cfg.ReadyPollTimeout = 10 * time.Second
	}

	// 1. Resolve the upstream address — either explicit or via docker.
	remoteAddr := cfg.Upstream
	if cfg.Container != "" {
		ip, err := resolveContainerIP(ctx, cfg.Via, cfg.Container, cfg.DockerNetwork)
		if err != nil {
			return nil, fmt.Errorf("resolve container %q on network %q via %q: %w",
				cfg.Container, cfg.DockerNetwork, cfg.Via, err)
		}
		remoteAddr = fmt.Sprintf("%s:%d", ip, cfg.RemotePort)
	}
	if remoteAddr == "" {
		return nil, errors.New("either --upstream or --container must be set when --via is used")
	}

	// 2. Pick a free local port. Brief race window between Close and ssh
	// binding, but acceptable for a dev-machine tool.
	localPort, err := pickFreePort()
	if err != nil {
		return nil, fmt.Errorf("pick free port: %w", err)
	}
	localAddr := fmt.Sprintf("127.0.0.1:%d", localPort)

	// 3. Spawn ssh as a child process. Note: -N (no remote command) + foreground
	// so we can manage the lifecycle. -o ExitOnForwardFailure=yes makes ssh exit
	// immediately if the forward can't be set up. -o ServerAliveInterval keeps
	// the tunnel from silently dying on flaky networks.
	args := []string{
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-o", fmt.Sprintf("ConnectTimeout=%d", int(cfg.ConnectTimeout.Seconds())),
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-L", fmt.Sprintf("%s:%s", localAddr, remoteAddr),
		cfg.Via,
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh: %w", err)
	}

	// 4. Wait for the local side to become connectable.
	if err := waitForListener(ctx, localAddr, cfg.ReadyPollTimeout); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("ssh tunnel never became ready: %w (check that '%s' resolves and that you can ssh to it)", err, cfg.Via)
	}

	return &SSHTunnel{cmd: cmd, LocalAddr: localAddr}, nil
}

// Close terminates the ssh subprocess and waits for it to exit.
func (t *SSHTunnel) Close() error {
	if t == nil || t.cmd == nil || t.cmd.Process == nil {
		return nil
	}
	_ = t.cmd.Process.Kill()
	return t.cmd.Wait()
}

// resolveContainerIP runs `ssh user@host docker inspect ...` to get the
// container's IP on the named Docker network. Format string mirrors the
// Kit Makefile pattern (`docker inspect -f '{{(index .NetworkSettings.Networks "<net>").IPAddress}}'`).
func resolveContainerIP(ctx context.Context, sshTarget, container, network string) (string, error) {
	tmpl := fmt.Sprintf(`{{(index .NetworkSettings.Networks "%s").IPAddress}}`, network)
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "ConnectTimeout=15",
		sshTarget,
		"docker", "inspect", "-f", tmpl, container,
	)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("ssh docker inspect: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("ssh docker inspect: %w", err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("container %q has no IP on network %q (is it running?)", container, network)
	}
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("docker returned non-IP value %q", ip)
	}
	return ip, nil
}

// pickFreePort listens on :0 to let the kernel pick a free port, then
// closes immediately and returns the assigned number.
func pickFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// waitForListener polls a TCP address until a connection succeeds or the
// deadline expires.
func waitForListener(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s: %w", addr, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

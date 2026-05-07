//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/ephpm/ephemerd/pkg/scheduler"
	"github.com/urfave/cli/v3"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func jobSubcommands() []*cli.Command {
	return []*cli.Command{
		jobKillCmd(),
		jobLogsCmd(),
		jobSSHCmd(),
	}
}

func jobSSHCmd() *cli.Command {
	return &cli.Command{
		Name:      "ssh",
		Usage:     "SSH into a running macOS VM job",
		ArgsUsage: "<job-id>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() == 0 {
				return fmt.Errorf("job ID required")
			}
			jobID, err := strconv.ParseInt(cmd.Args().First(), 10, 64)
			if err != nil {
				return fmt.Errorf("invalid job id: %w", err)
			}

			// Connect to the daemon's HTTP unix socket to get SSH info
			socketPath := scheduler.SocketPath(configDir) + ".http"
			httpClient := &http.Client{
				Transport: &http.Transport{
					DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
						return net.Dial("unix", socketPath)
					},
				},
			}

			resp, err := httpClient.Get(fmt.Sprintf("http://ephemerd/vm/ssh-info?job_id=%d", jobID))
			if err != nil {
				return fmt.Errorf("cannot connect to ephemerd (is it running?): %w", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("ephemerd: %s", body)
			}

			var info scheduler.VMSSHInfo
			if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
				return fmt.Errorf("decoding SSH info: %w", err)
			}

			if info.IP == "" {
				return fmt.Errorf("VM IP not yet available")
			}

			// Parse the ephemeral private key
			signer, err := ssh.ParsePrivateKey(info.PrivateKey)
			if err != nil {
				return fmt.Errorf("parsing SSH key: %w", err)
			}

			// SSH into the VM using the ephemeral key — no password auth
			config := &ssh.ClientConfig{
				User: info.User,
				Auth: []ssh.AuthMethod{
					ssh.PublicKeys(signer),
				},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			}

			client, err := ssh.Dial("tcp", info.IP+":22", config)
			if err != nil {
				return fmt.Errorf("SSH dial %s: %w", info.IP, err)
			}
			defer func() { _ = client.Close() }()

			session, err := client.NewSession()
			if err != nil {
				return fmt.Errorf("SSH session: %w", err)
			}
			defer func() { _ = session.Close() }()

			// Set up terminal
			session.Stdin = os.Stdin
			session.Stdout = os.Stdout
			session.Stderr = os.Stderr

			// Put terminal in raw mode
			fd := int(os.Stdin.Fd())
			if term.IsTerminal(fd) {
				oldState, err := term.MakeRaw(fd)
				if err == nil {
					defer func() { _ = term.Restore(fd, oldState) }()
				}

				w, h, _ := term.GetSize(fd)
				if err := session.RequestPty("xterm-256color", h, w, ssh.TerminalModes{
					ssh.ECHO:          1,
					ssh.TTY_OP_ISPEED: 14400,
					ssh.TTY_OP_OSPEED: 14400,
				}); err != nil {
					return fmt.Errorf("requesting PTY: %w", err)
				}

				// Handle window resize
				sigCh := make(chan os.Signal, 1)
				signal.Notify(sigCh, syscall.SIGWINCH)
				go func() {
					for range sigCh {
						w, h, _ := term.GetSize(fd)
						_ = session.WindowChange(h, w)
					}
				}()
			}

			if err := session.Shell(); err != nil {
				return fmt.Errorf("starting shell: %w", err)
			}

			return session.Wait()
		},
	}
}

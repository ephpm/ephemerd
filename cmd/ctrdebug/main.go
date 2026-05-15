package main

// Diagnostic tool: connect to the Linux VM's containerd over TCP, list
// running containers, exec commands, or dump logs. Exec uses the in-VM
// debug-exec HTTP endpoint on containerd_port+2 because containerd's cio
// FIFOs can't bridge Windows host ↔ Linux VM (named pipes vs Unix FIFOs).
//
// Usage:
//   ctrdebug list [--addr host:port] [--ns namespace]
//   ctrdebug exec <container-id-or-prefix> -- /bin/sh -c 'commands'

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	addr = flag.String("addr", "192.168.64.18:10000", "containerd TCP endpoint")
	ns   = flag.String("ns", "ephemerd", "containerd namespace")
)

func newClient() *client.Client {
	grpcConn, err := grpc.NewClient(*addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(defaults.DefaultMaxRecvMsgSize),
			grpc.MaxCallSendMsgSize(defaults.DefaultMaxSendMsgSize),
		),
	)
	if err != nil {
		panic(err)
	}
	c, err := client.NewWithConn(grpcConn)
	if err != nil {
		panic(err)
	}
	return c
}

// debugExecHost returns the host:port for the in-VM debug-exec HTTP server,
// derived from --addr by incrementing the containerd port by 2 (matches the
// worker-mode debugCleanup wiring in cmd/ephemerd/main.go).
func debugExecHost() string {
	host, port, ok := strings.Cut(*addr, ":")
	if !ok {
		return *addr
	}
	var portNum int
	if _, err := fmt.Sscanf(port, "%d", &portNum); err != nil {
		return *addr
	}
	return fmt.Sprintf("%s:%d", host, portNum+2)
}

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		fmt.Println("usage: ctrdebug [flags] list | exec <id> -- <cmd...>")
		os.Exit(1)
	}

	switch args[0] {
	case "list":
		c := newClient()
		defer func() { _ = c.Close() }()
		for _, nsName := range []string{*ns, "default", "k8s.io"} {
			nsCtx := namespaces.WithNamespace(context.Background(), nsName)
			cs, err := c.Containers(nsCtx)
			if err != nil {
				fmt.Printf("namespace=%s: list error %v\n", nsName, err)
				continue
			}
			fmt.Printf("namespace=%s containers=%d\n", nsName, len(cs))
			for _, cnt := range cs {
				info, _ := cnt.Info(nsCtx)
				task, terr := cnt.Task(nsCtx, nil)
				status := "no-task"
				if terr == nil {
					s, _ := task.Status(nsCtx)
					status = fmt.Sprintf("task pid=%d status=%s", task.Pid(), s.Status)
				}
				fmt.Printf("  %s  image=%s  %s\n", cnt.ID(), info.Image, status)
			}
		}
	case "exec":
		if len(args) < 3 || args[2] != "--" {
			fmt.Println("usage: ctrdebug exec <id> -- <cmd...>")
			os.Exit(1)
		}
		if err := runExec(args[1], args[3:]); err != nil {
			fmt.Fprintf(os.Stderr, "exec failed: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Println("unknown subcommand:", args[0])
		os.Exit(1)
	}
}

func runExec(cid string, cmd []string) error {
	cmdJSON, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("encode cmd: %w", err)
	}

	u := url.URL{
		Scheme: "http",
		Host:   debugExecHost(),
		Path:   "/exec",
	}
	q := u.Query()
	q.Set("ns", *ns)
	q.Set("cid", cid)
	q.Set("cmd", string(cmdJSON))
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("debug-exec: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("debug-exec returned %d: %s", resp.StatusCode, body)
	}
	if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
		return fmt.Errorf("streaming output: %w", err)
	}
	return nil
}

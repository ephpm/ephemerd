package main

// Diagnostic tool: connect to the Linux VM's containerd over TCP, list
// running containers, exec commands, or dump logs.
//
// Usage:
//   ctrdebug list [--addr host:port] [--ns namespace]
//   ctrdebug exec <container-id> -- /bin/sh -c 'commands'
//   ctrdebug exec-all <search> -- <cmd...>

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	specs "github.com/opencontainers/runtime-spec/specs-go"
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

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		fmt.Println("usage: ctrdebug [flags] list | exec <id> -- <cmd...> | exec-all <substr> -- <cmd...>")
		os.Exit(1)
	}

	c := newClient()
	defer func() { _ = c.Close() }()
	ctx := namespaces.WithNamespace(context.Background(), *ns)

	switch args[0] {
	case "list":
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
		runExec(ctx, c, args[1], args[3:])
	case "exec-all":
		if len(args) < 3 || args[2] != "--" {
			fmt.Println("usage: ctrdebug exec-all <substr> -- <cmd...>")
			os.Exit(1)
		}
		substr := args[1]
		cs, _ := c.Containers(ctx)
		n := 0
		for _, cnt := range cs {
			if !strings.Contains(cnt.ID(), substr) {
				continue
			}
			n++
			fmt.Printf("=== %s ===\n", cnt.ID())
			runExec(ctx, c, cnt.ID(), args[3:])
		}
		fmt.Fprintf(os.Stderr, "matched %d containers\n", n)
	default:
		fmt.Println("unknown subcommand:", args[0])
		os.Exit(1)
	}
}

func runExec(ctx context.Context, c *client.Client, id string, cmd []string) {
	cnt, err := c.LoadContainer(ctx, id)
	if err != nil {
		fmt.Printf("load: %v\n", err)
		return
	}
	task, err := cnt.Task(ctx, nil)
	if err != nil {
		fmt.Printf("task: %v\n", err)
		return
	}
	spec, err := cnt.Spec(ctx)
	if err != nil {
		fmt.Printf("spec: %v\n", err)
		return
	}
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	if spec.Process != nil {
		env = spec.Process.Env
	}
	pspec := specs.Process{
		Args: cmd,
		Env:  env,
		Cwd:  "/",
		User: specs.User{UID: 0, GID: 0},
	}
	execID := fmt.Sprintf("diag-%d", time.Now().UnixNano())
	// The shim runs inside the VM and writes to a path it can see. Use the
	// VM-side virtio-fs mount path; the host-side equivalent is the same
	// directory under /var/lib/ephemerd.
	const vmShare = "/mnt/ephemerd/debug"
	const hostShare = "/var/lib/ephemerd/debug"
	_ = os.MkdirAll(hostShare, 0o755)
	vmLogPath := vmShare + "/" + execID + ".log"
	hostLogPath := hostShare + "/" + execID + ".log"
	proc, err := task.Exec(ctx, execID, &pspec, cio.LogFile(vmLogPath))
	if err != nil {
		fmt.Printf("exec create: %v\n", err)
		return
	}
	defer func() { _, _ = proc.Delete(ctx) }()
	statusC, err := proc.Wait(ctx)
	if err != nil {
		fmt.Printf("exec wait: %v\n", err)
		return
	}
	if err := proc.Start(ctx); err != nil {
		fmt.Printf("exec start: %v\n", err)
		return
	}
	status := <-statusC
	// Print captured log
	if data, err := os.ReadFile(hostLogPath); err == nil {
		_, _ = os.Stdout.Write(data)
	} else {
		fmt.Fprintf(os.Stderr, "(no log at %s: %v)\n", hostLogPath, err)
	}
	_ = os.Remove(hostLogPath)
	fmt.Fprintf(os.Stderr, "--- exit=%d ---\n", status.ExitCode())
}

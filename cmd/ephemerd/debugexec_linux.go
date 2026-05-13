//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// startWorkerDebugExec is the Linux build of the helper called from
// worker-mode main. Wraps startDebugExecServer.
func startWorkerDebugExec(ctx context.Context, port int, ctrdClient *client.Client, log *slog.Logger) func() {
	return startDebugExecServer(ctx, port, ctrdClient, log)
}

// startDebugExecServer launches a tiny HTTP server that lets a remote
// (Windows host) caller exec a command inside any container in any
// containerd namespace and stream stdout/stderr back. Containerd's cio
// FIFO IO can't cross VM boundaries (the shim would have to open Windows
// named pipes), so we run the exec locally inside the VM where the shim
// is and pipe its IO directly into the HTTP response. Plain text protocol,
// not exposed beyond the dispatch port — intended for diagnostics like
// `ctrdebug exec` rather than user-facing API.
//
//	GET /exec?ns=<containerd-ns>&cid=<container-id-or-prefix>&cmd=<base64-json-array>
//	Returns: stdout bytes, then a trailer "\n--- exit=<code> ---\n" on stderr
func startDebugExecServer(ctx context.Context, port int, ctrdClient *client.Client, log *slog.Logger) func() {
	// writef wraps fmt.Fprintf for response writes — a broken HTTP connection
	// is the only realistic source of failure here, and there's nothing
	// useful to do beyond logging at debug level.
	writef := func(w http.ResponseWriter, format string, args ...any) {
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			log.Debug("debugexec response write", "error", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/namespaces", func(w http.ResponseWriter, r *http.Request) {
		nss, err := ctrdClient.NamespaceService().List(r.Context())
		if err != nil {
			http.Error(w, "list namespaces: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		for _, n := range nss {
			writef(w, "%s\n", n)
		}
	})
	mux.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) {
		ns := r.URL.Query().Get("ns")
		w.Header().Set("Content-Type", "text/plain")
		var targets []string
		if ns == "*" || ns == "" {
			nss, err := ctrdClient.NamespaceService().List(r.Context())
			if err != nil {
				http.Error(w, "list namespaces: "+err.Error(), http.StatusInternalServerError)
				return
			}
			targets = nss
		} else {
			targets = []string{ns}
		}
		for _, target := range targets {
			nsCtx := namespaces.WithNamespace(r.Context(), target)
			all, err := ctrdClient.Containers(nsCtx)
			if err != nil {
				writef(w, "namespace=%s error=%v\n", target, err)
				continue
			}
			writef(w, "namespace=%s count=%d\n", target, len(all))
			for _, c := range all {
				info, ierr := c.Info(nsCtx)
				if ierr != nil {
					writef(w, "  %s info-error=%v\n", c.ID(), ierr)
					continue
				}
				writef(w, "  %s image=%s\n", c.ID(), info.Image)
			}
		}
	})

	mux.HandleFunc("/exec", func(w http.ResponseWriter, r *http.Request) {
		ns := r.URL.Query().Get("ns")
		cid := r.URL.Query().Get("cid")
		cmdRaw := r.URL.Query().Get("cmd")
		if ns == "" || cid == "" || cmdRaw == "" {
			http.Error(w, "missing ns/cid/cmd", http.StatusBadRequest)
			return
		}
		var argv []string
		if err := json.Unmarshal([]byte(cmdRaw), &argv); err != nil || len(argv) == 0 {
			http.Error(w, "cmd must be a non-empty JSON array of strings", http.StatusBadRequest)
			return
		}

		nsCtx := namespaces.WithNamespace(r.Context(), ns)
		// Allow prefix match against the container ID — convenient for callers
		// who only know the first 12 hex chars from `ctrdebug list`.
		var cnt client.Container
		all, err := ctrdClient.Containers(nsCtx)
		if err != nil {
			http.Error(w, "list containers: "+err.Error(), http.StatusInternalServerError)
			return
		}
		for _, c := range all {
			if strings.HasPrefix(c.ID(), cid) {
				cnt = c
				break
			}
		}
		if cnt == nil {
			ids := make([]string, 0, len(all))
			for _, c := range all {
				ids = append(ids, c.ID())
			}
			http.Error(w,
				fmt.Sprintf("container with prefix %q not found in namespace %s (saw %d containers: %v)",
					cid, ns, len(all), ids),
				http.StatusNotFound)
			return
		}

		task, err := cnt.Task(nsCtx, nil)
		if err != nil {
			http.Error(w, "load task: "+err.Error(), http.StatusInternalServerError)
			return
		}
		spec, err := cnt.Spec(nsCtx)
		if err != nil {
			http.Error(w, "load spec: "+err.Error(), http.StatusInternalServerError)
			return
		}
		env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
		if spec.Process != nil && len(spec.Process.Env) > 0 {
			env = spec.Process.Env
		}
		pspec := specs.Process{
			Args: argv,
			Env:  env,
			Cwd:  "/",
			User: specs.User{UID: 0, GID: 0},
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Trailer", "X-Exit-Code")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		execID := fmt.Sprintf("debugexec-%d", time.Now().UnixNano())
		// stdout/stderr are written directly into the HTTP response. We're
		// inside the VM here, so cio.NewCreator + WithStreams uses local
		// FIFOs that the shim (also local) opens without issue.
		proc, err := task.Exec(nsCtx, execID, &pspec, cio.NewCreator(cio.WithStreams(nil, w, w)))
		if err != nil {
			writef(w, "\n--- exec create failed: %v ---\n", err)
			return
		}
		defer func() {
			if _, derr := proc.Delete(nsCtx); derr != nil {
				log.Debug("debugexec: process delete", "error", derr)
			}
		}()
		statusCh, err := proc.Wait(nsCtx)
		if err != nil {
			writef(w, "\n--- wait failed: %v ---\n", err)
			return
		}
		if err := proc.Start(nsCtx); err != nil {
			writef(w, "\n--- start failed: %v ---\n", err)
			return
		}
		st := <-statusCh
		// Drain any in-flight IO.
		if procIO := proc.IO(); procIO != nil {
			procIO.Wait()
			if cerr := procIO.Close(); cerr != nil {
				log.Debug("debugexec: cio close", "error", cerr)
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
		w.Header().Set("X-Exit-Code", fmt.Sprintf("%d", st.ExitCode()))
		writef(w, "\n--- exit=%d ---\n", st.ExitCode())
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		log.Warn("debug-exec server failed to listen", "port", port, "error", err)
		return func() {}
	}
	log.Info("debug-exec server listening", "port", port)
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Warn("debug-exec server stopped", "error", err)
		}
	}()
	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
}

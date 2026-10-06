/*
Copyright paul_wtf.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/spawnery/spawnery/internal/version"
	"github.com/spawnery/spawnery/internal/worldsync"
)

func main() { os.Exit(run(os.Args[1:], os.Getenv, os.Stderr)) }

func run(args []string, getenv func(string) string, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: spawnery-worldsync node|import [flags]")
		return 2
	}
	switch args[0] {
	case "node":
		return runNode(args[1:], getenv, stderr)
	case "import":
		return runImport(args[1:], getenv, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "spawnery-worldsync: unknown subcommand %q\n", args[0])
		return 2
	}
}

func store(getenv func(string) string, stderr io.Writer) (worldsync.Store, string, bool) {
	cfg, base, err := worldsync.S3ConfigFromEnv(getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "spawnery-worldsync: %v\n", err)
		return nil, "", false
	}
	st, err := worldsync.NewS3Store(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "spawnery-worldsync: %v\n", err)
		return nil, "", false
	}
	return st, base, true
}

func runImport(args []string, getenv func(string) string, stderr io.Writer) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	world := fs.String("world", "", "<namespace>/<group>/<key>")
	dir := fs.String("dir", "", "the directory holding /data's content")
	keep := fs.String("keep", "", "spec.storage.keep, one entry per line")
	if err := fs.Parse(args); err != nil || *world == "" || *dir == "" || *keep == "" {
		_, _ = fmt.Fprintln(stderr, "spawnery-worldsync import: --world, --dir and --keep are required")
		return 2
	}
	st, base, ok := store(getenv, stderr)
	if !ok {
		return 1
	}
	m, err := worldsync.Import(context.Background(), st, base, *world, strings.Split(*keep, "\n"), *dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "spawnery-worldsync import: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "imported %s: %d files, world %s\n", *world, len(m.Files), m.WorldID)
	return 0
}

func runNode(args []string, getenv func(string) string, stderr io.Writer) int {
	fs := flag.NewFlagSet("node", flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", "unix:///csi/csi.sock", "CSI endpoint")
	nodeID := fs.String("node-id", getenv("NODE_NAME"), "this node's name")
	root := fs.String("root", "/var/lib/spawnery/worldsync", "the node directory for worlds")
	metrics := fs.String("metrics-bind-address", ":8090", "metrics endpoint")
	opts := zap.Options{}
	opts.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := zap.New(zap.UseFlagOptions(&opts))
	ctrl.SetLogger(log)
	st, base, ok := store(getenv, stderr)
	if !ok {
		return 1
	}
	node, err := worldsync.NewNode(worldsync.Config{
		Root: *root, NodeID: *nodeID, Base: base, Store: st, Mounter: worldsync.BindMounter{},
		StaleAfter: worldsync.StaleAfter, RenewEvery: 30 * time.Second, PollEvery: 500 * time.Millisecond,
		EvictAfter: 24 * time.Hour, Parallel: 32, MinFree: 0.15, Clock: time.Now, Log: log,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "spawnery-worldsync: %v\n", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := node.Resume(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "spawnery-worldsync: resume: %v\n", err)
		return 1
	}
	go node.Run(ctx)

	reg := prometheus.NewRegistry()
	reg.MustRegister(worldsync.Collectors()...)
	go func() {
		srv := &http.Server{Addr: *metrics, Handler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), ReadHeaderTimeout: 10 * time.Second}
		_ = srv.ListenAndServe()
	}()

	path := strings.TrimPrefix(*endpoint, "unix://")
	_ = os.Remove(path)
	lis, err := net.Listen("unix", path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "spawnery-worldsync: listen %s: %v\n", path, err)
		return 1
	}
	srv := grpc.NewServer()
	cs := worldsync.NewCSIServer(node, *nodeID, version.Version)
	csi.RegisterIdentityServer(srv, cs)
	csi.RegisterNodeServer(srv, cs)
	go func() { <-ctx.Done(); srv.GracefulStop() }()
	if err := srv.Serve(lis); err != nil {
		_, _ = fmt.Fprintf(stderr, "spawnery-worldsync: serve: %v\n", err)
		return 1
	}
	return 0
}

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/mcp"
	"github.com/agent-substrate/env/internal/portproxy"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
)

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func main() {
	var (
		listen            = flag.String("listen", "0.0.0.0:7777", "address to serve the API on")
		ateapi            = flag.String("ateapi", ate.DefaultControlAddr, "address of the ateapi gRPC control plane")
		atenet            = flag.String("atenet", ate.DefaultRouterAddr, "address of the atenet HTTP router")
		atenetConn        = flag.String("atenet-connect", ate.DefaultRouterConnectAddr, "address of the atenet router's plaintext CONNECT listener; HTTP requests carrying X-Env-Id and X-Env-Port, or to /v1alpha/envs/{id}/ports/{port}/..., are forwarded through it to that port in the environment. Without -auth-token-file the forwarding does not authenticate callers, and it reaches any port of any actor the router serves. Empty disables it")
		ppOrigins         = flag.String("port-proxy-trusted-origins", "", "comma-separated browser origins (scheme://host[:port]) whose pages may use the port proxy's path form, next to the listener's own origin")
		ppHeaderTimeout   = flag.Duration("port-proxy-response-header-timeout", 0, "how long the port proxy waits for a daemon's response headers; 0 means no limit, and the caller's deadline applies")
		hostSuffix        = flag.String("host-suffix", ate.DefaultHostSuffix, "atenet router host suffix for actor routing")
		skipVerify        = flag.Bool("skip-verify", true, "skip TLS certificate verification on the control plane connection")
		tokenFile         = flag.String("ateapi-token-file", "", "file with a bearer token for the control plane, e.g. a projected ServiceAccount token with the ateapi audience")
		authTokenFile     = flag.String("auth-token-file", "", "file with a bearer token required from gRPC, MCP and port-proxy callers; re-read when the file changes")
		wakeBudget        = flag.Duration("wake-budget", apiservice.DefaultWakeBudget, "how long a guest call or a port-proxy request waits for a suspended environment to wake before failing; cold image pulls and cross-region snapshot reads exceed the router's own parking budget")
		wakeRunningBudget = flag.Duration("wake-running-budget", apiservice.DefaultWakeRunningBudget, "how long a guest call waits when the environment is already RUNNING but the router still answers Unavailable (guest not yet listening vs. path down)")
	)
	flag.Parse()

	var callerAuth *apiservice.BearerAuth
	if *authTokenFile != "" {
		var err error
		callerAuth, err = apiservice.NewBearerAuthFromFile(*authTokenFile)
		if err != nil {
			log.Fatalf("loading --auth-token-file: %v", err)
		}
	} else if !isLoopbackAddr(*listen) {
		log.Printf("WARNING: listening on %s without --auth-token-file; all gRPC, MCP and port-proxy callers are unauthenticated", *listen)
	}

	client, err := ate.New(ate.Options{
		ControlAddr:     *ateapi,
		RouterAddr:      *atenet,
		HostSuffix:      *hostSuffix,
		SkipVerify:      *skipVerify,
		BearerTokenFile: *tokenFile,
	})
	if err != nil {
		log.Fatalf("creating env client: %v", err)
	}
	defer client.Close()

	// Create the default atespace up front so ActorTemplates can be registered
	// before the first environment is created (templates live in an atespace).
	// Bounded so a hung control plane fails the pod instead of leaving it
	// unlistening; the restart backoff retries until Substrate answers.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := client.EnsureAtespace(ctx, ate.DefaultAtespace); err != nil {
		log.Fatalf("ensuring default atespace %q: %v", ate.DefaultAtespace, err)
	}
	cancel()
	log.Printf("default atespace %q ready", ate.DefaultAtespace)

	apisvc := apiservice.New(client, *atenet, *hostSuffix)
	apisvc.WakeBudget = *wakeBudget
	apisvc.WakeRunningBudget = *wakeRunningBudget
	defer apisvc.Close()

	// Authentication runs first, so an unauthenticated caller learns nothing
	// from the unknown-field check.
	var unary []grpc.UnaryServerInterceptor
	var stream []grpc.StreamServerInterceptor
	if callerAuth != nil {
		unary = append(unary, callerAuth.UnaryInterceptor())
		stream = append(stream, callerAuth.StreamInterceptor())
	}
	unary = append(unary, apiservice.RejectUnknownFieldsUnaryInterceptor())
	stream = append(stream, apiservice.RejectUnknownFieldsStreamInterceptor())
	grpcServer := grpc.NewServer(grpc.ChainUnaryInterceptor(unary...), grpc.ChainStreamInterceptor(stream...))
	ateenvv1alpha.RegisterEnvironmentServiceServer(grpcServer, apisvc)
	ateenvv1alpha.RegisterProcessServiceServer(grpcServer, apisvc)
	ateenvv1alpha.RegisterFileSystemServiceServer(grpcServer, apisvc)

	mcpHandler := mcp.NewHandler(client)
	if callerAuth != nil {
		mcpHandler = callerAuth.WrapHTTP(mcpHandler)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.Handle("/v1alpha/envs/{id}/mcp", mcpHandler)
	mux.Handle("/", grpcServer)

	// Requests that name an environment port go to the port proxy before
	// anything else, whatever their path: the path belongs to the daemon.
	var handler http.Handler = mux
	if *atenetConn != "" {
		var origins []string
		for o := range strings.SplitSeq(*ppOrigins, ",") {
			if o = strings.TrimSpace(o); o != "" {
				origins = append(origins, o)
			}
		}
		ppOpts := portproxy.Options{
			ConnectAddr:           *atenetConn,
			HostSuffix:            *hostSuffix,
			DefaultAtespace:       ate.DefaultAtespace,
			ResponseHeaderTimeout: *ppHeaderTimeout,
			TrustedOrigins:        origins,
			WakeBudget:            *wakeBudget,
		}
		if callerAuth != nil {
			ppOpts.Authenticate = callerAuth.Check
		}
		pp, err := portproxy.New(ppOpts)
		if err != nil {
			log.Fatalf("--port-proxy-trusted-origins: %v", err)
		}
		// Header form and path form, /v1alpha/envs/{id}/ports/{port}/...
		handler = portproxy.Dispatch(pp, mux)
	}

	// Enable both HTTP/1.1 (for /healthz and /v1alpha/ HTTP endpoints) and
	// unencrypted HTTP/2 (h2c, for gRPC services) on the same plaintext TCP listener.
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	srv := &http.Server{
		Addr:      *listen,
		Handler:   handler,
		Protocols: &protocols,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
		_ = srv.Shutdown(context.Background())
	}()

	log.Printf("ate-env-api listening on %s (ateapi %s, atenet %s, atenet CONNECT %q)", *listen, *ateapi, *atenet, *atenetConn)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

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

package apiservice

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"unicode"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// BearerAuth authenticates incoming gRPC and HTTP callers against an expected
// bearer token, loaded either from a static string or from a token file. The
// file is read again when it changes, so rotated Secret mounts take effect
// without a restart.
type BearerAuth struct {
	token     string
	tokenFile string

	mu     sync.Mutex
	cached string      // the token last read from tokenFile
	stat   os.FileInfo // tokenFile's stat when cached was read
}

// NewBearerAuth creates a BearerAuth that requires the given static token.
func NewBearerAuth(token string) (*BearerAuth, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("apiservice: bearer token must not be empty")
	}
	return &BearerAuth{token: token}, nil
}

// NewBearerAuthFromFile creates a BearerAuth backed by path, validating at
// construction time that path is readable and non-empty.
func NewBearerAuthFromFile(path string) (*BearerAuth, error) {
	if path == "" {
		return nil, errors.New("apiservice: bearer token file path is required")
	}
	auth := &BearerAuth{tokenFile: path}
	if _, err := auth.expectedToken(); err != nil {
		return nil, err
	}
	return auth, nil
}

// expectedToken returns the static token, or the token file's content. Each
// check stats the file, and reads it only when the stat changed: another file
// in its place (a Kubernetes Secret update swaps a symlink), a new
// modification time or a new size. An in-place rewrite that keeps the size and
// the modification time is missed, for example a token of the same length
// written within one second on a filesystem with one-second timestamps. Secret
// updates replace the file, which the cache detects.
func (a *BearerAuth) expectedToken() (string, error) {
	if a.tokenFile == "" {
		return a.token, nil
	}
	fi, err := os.Stat(a.tokenFile)
	if err != nil {
		return "", fmt.Errorf("apiservice: reading bearer token file %q: %w", a.tokenFile, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stat != nil && os.SameFile(a.stat, fi) && a.stat.ModTime().Equal(fi.ModTime()) && a.stat.Size() == fi.Size() {
		return a.cached, nil
	}
	b, err := os.ReadFile(a.tokenFile)
	if err != nil {
		return "", fmt.Errorf("apiservice: reading bearer token file %q: %w", a.tokenFile, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("apiservice: bearer token file %q is empty", a.tokenFile)
	}
	a.cached, a.stat = tok, fi
	return tok, nil
}

// errVerify is what a caller gets when the server cannot load its own token.
// The details name the token file, thus they go to the server log only.
var errVerify = status.Error(codes.Internal, "internal error")

func (a *BearerAuth) checkHeader(authHeader string) error {
	got, ok := bearerToken(authHeader)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
	}
	want, err := a.expectedToken()
	if err != nil {
		log.Printf("apiservice: verifying bearer token: %v", err)
		return errVerify
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
	}
	return nil
}

// bearerToken returns the token of an Authorization value "Bearer <token>".
// The scheme is case-insensitive (RFC 7235), and any whitespace can separate
// it from the token.
func bearerToken(value string) (string, bool) {
	value = strings.TrimSpace(value)
	i := strings.IndexFunc(value, unicode.IsSpace)
	if i < 0 || !strings.EqualFold(value[:i], "Bearer") {
		return "", false
	}
	tok := strings.TrimSpace(value[i:])
	return tok, tok != ""
}

// Check reports whether value, an Authorization header value, carries the
// expected bearer token. The port proxy calls it for forwarded requests.
func (a *BearerAuth) Check(value string) error {
	return a.checkHeader(value)
}

func (a *BearerAuth) authorizeContext(ctx context.Context) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
	}
	return a.checkHeader(vals[0])
}

// UnaryInterceptor returns a gRPC UnaryServerInterceptor that enforces
// Authorization: Bearer <token>.
func (a *BearerAuth) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := a.authorizeContext(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor returns a gRPC StreamServerInterceptor that enforces
// Authorization: Bearer <token>.
func (a *BearerAuth) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := a.authorizeContext(ss.Context()); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// WrapHTTP wraps next with HTTP middleware that enforces Authorization: Bearer <token>.
func (a *BearerAuth) WrapHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := a.checkHeader(r.Header.Get("Authorization")); err != nil {
			if status.Code(err) == codes.Internal {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "missing or invalid bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

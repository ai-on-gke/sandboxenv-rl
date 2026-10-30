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

// Package fakecontrol implements an in-memory fake of the Substrate ateapi
// Control service for tests. It mimics the control plane's lifecycle
// semantics: actors are created suspended, resume/suspend/pause flip
// status, and, like Substrate v0.6.0, an actor in any state can be deleted.
package fakecontrol

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Server is a fake ateapi Control server.
type Server struct {
	ateapipb.UnimplementedControlServer

	mu        sync.Mutex
	actors    map[string]*ateapipb.Actor
	atespaces map[string]*ateapipb.Atespace
	// DenyCreateAtespace makes CreateAtespace fail with PermissionDenied, as
	// Substrate does for an identity allowed to use but not create atespaces.
	DenyCreateAtespace bool
	templates          map[string]*ateapipb.ActorTemplate
	templateSeq        int
	// RequireTemplate makes CreateActor fail with FailedPrecondition when
	// the actor's template does not exist, as Substrate does.
	RequireTemplate bool
	// DeleteTemplateErr, when set, is the error of each DeleteActorTemplate.
	DeleteTemplateErr error
	// templateLeases are the templates whose lease a test holds, as the
	// template reconciler or a running delete does.
	templateLeases map[string]bool
	// templateDeletes and templateCreates count the DeleteActorTemplate
	// and CreateActorTemplate calls.
	templateDeletes, templateCreates int
	// listCalls counts the ListActors calls.
	listCalls int
	// SuspendModes records the durability mode of each SuspendActor call.
	SuspendModes []ateapipb.DurabilityMode
}

// New returns an empty fake control server.
func New() *Server {
	return &Server{
		actors:    make(map[string]*ateapipb.Actor),
		atespaces: make(map[string]*ateapipb.Atespace),
		templates: make(map[string]*ateapipb.ActorTemplate),
	}
}

// AddTemplate seeds an ActorTemplate, as an operator would have registered it.
func (s *Server) AddTemplate(tmpl *ateapipb.ActorTemplate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.templates[key(tmpl.GetMetadata().GetAtespace(), tmpl.GetMetadata().GetName())] = proto.Clone(tmpl).(*ateapipb.ActorTemplate)
}

// Template returns a copy of a stored ActorTemplate, or nil.
func (s *Server) Template(atespace, name string) *ateapipb.ActorTemplate {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.templates[key(atespace, name)]
	if !ok {
		return nil
	}
	return proto.Clone(t).(*ateapipb.ActorTemplate)
}

// TemplateCount is how many ActorTemplates are stored.
func (s *Server) TemplateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.templates)
}

func (s *Server) GetActorTemplate(ctx context.Context, req *ateapipb.GetActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := req.GetActorTemplate()
	t, ok := s.templates[key(ref.GetAtespace(), ref.GetName())]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "actor template %q not found", ref.GetName())
	}
	return proto.Clone(t).(*ateapipb.ActorTemplate), nil
}

// HoldTemplateLease holds the lease of a template until release is called:
// meanwhile DeleteActorTemplate on it fails with Aborted, as in Substrate
// while the template reconciler or another delete holds it.
func (s *Server) HoldTemplateLease(atespace, name string) (release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.templateLeases == nil {
		s.templateLeases = make(map[string]bool)
	}
	s.templateLeases[key(atespace, name)] = true
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.templateLeases, key(atespace, name))
	}
}

// ListCalls returns how many ListActors calls the fake got.
func (s *Server) ListCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls
}

// MutateActor applies f to the stored actor with the given name, for tests
// that need a status the fake's lifecycle does not produce.
func (s *Server) MutateActor(name string, f func(*ateapipb.Actor)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.find(name); a != nil {
		f(a)
	}
}

// TemplateCalls returns how many DeleteActorTemplate and CreateActorTemplate
// calls the fake got.
func (s *Server) TemplateCalls() (deletes, creates int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.templateDeletes, s.templateCreates
}

func (s *Server) CreateActorTemplate(ctx context.Context, req *ateapipb.CreateActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.templateCreates++
	t := req.GetActorTemplate()
	md := t.GetMetadata()
	if md.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "metadata.name is required")
	}
	k := key(md.GetAtespace(), md.GetName())
	if _, exists := s.templates[k]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "actor template %q already exists", md.GetName())
	}
	// Mirror the server's volume_mounts validation so a bad derivation fails here too.
	declared := map[string]bool{}
	for _, v := range t.GetVolumes() {
		declared[v.GetName()] = true
	}
	for _, c := range t.GetContainers() {
		for _, m := range c.GetVolumeMounts() {
			if !declared[m.GetName()] {
				return nil, status.Errorf(codes.InvalidArgument, "container %q mounts undeclared volume %q", c.GetName(), m.GetName())
			}
		}
	}
	stored := proto.Clone(t).(*ateapipb.ActorTemplate)
	s.templateSeq++
	stored.Metadata.Uid = fmt.Sprintf("tmpl-%s-%d", md.GetName(), s.templateSeq)
	stored.Metadata.Version = 1
	s.templates[k] = stored
	return proto.Clone(stored).(*ateapipb.ActorTemplate), nil
}

// DeleteActorTemplate deletes a template, with Substrate's uid and version
// preconditions: a mismatch is Aborted.
func (s *Server) DeleteActorTemplate(ctx context.Context, req *ateapipb.DeleteActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.templateDeletes++
	if s.DeleteTemplateErr != nil {
		return nil, s.DeleteTemplateErr
	}
	ref := req.GetActorTemplate()
	k := key(ref.GetAtespace(), ref.GetName())
	if s.templateLeases[k] {
		return nil, status.Errorf(codes.Aborted, "another operation is in progress for this ActorTemplate %s", ref.GetName())
	}
	t, ok := s.templates[k]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "ActorTemplate %s not found", ref.GetName())
	}
	if uid := req.GetOptions().GetUid(); uid != "" && uid != t.GetMetadata().GetUid() {
		return nil, status.Errorf(codes.Aborted, "ActorTemplate %s does not have uid %s", ref.GetName(), uid)
	}
	if v := req.GetOptions().GetVersion(); v != 0 && v != t.GetMetadata().GetVersion() {
		return nil, status.Error(codes.Aborted, "concurrent update conflict, please retry")
	}
	delete(s.templates, k)
	return proto.Clone(t).(*ateapipb.ActorTemplate), nil
}

// Serve starts the fake on a random localhost port and returns its
// address and a shutdown function. Like the real ateapi, it serves TLS
// with a certificate no client can verify (clients set SkipVerify).
func (s *Server) Serve() (addr string, stop func(), err error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	cert, err := selfSignedCert()
	if err != nil {
		lis.Close()
		return "", nil, err
	}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
	})))
	ateapipb.RegisterControlServer(grpcServer, s)
	go grpcServer.Serve(lis)
	return lis.Addr().String(), grpcServer.Stop, nil
}

func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fakecontrol"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func key(atespace, name string) string { return atespace + "/" + name }

// Status returns the current state of the actor with the given name, or
// ACTOR_STATE_UNSPECIFIED if it does not exist.
func (s *Server) Status(name string) ateapipb.ActorState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.find(name).GetStatus().GetState()
}

// SetState overrides an actor's state, for tests that need a state the fake's
// own transitions never produce, such as CRASHED, or need to hold an actor
// SUSPENDED after creation.
func (s *Server) SetState(name string, state ateapipb.ActorState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.find(name)
	if a == nil {
		return
	}
	if a.Status == nil {
		a.Status = &ateapipb.ActorStatus{}
	}
	a.Status.State = state
}

// find returns the actor with the given name, whichever atespace holds it, or
// nil when no actor has that name. The helpers above address actors by name
// alone so tests do not have to know which atespace a client placed them in.
func (s *Server) find(name string) *ateapipb.Actor {
	for _, a := range s.actors {
		if a.GetMetadata().GetName() == name {
			return a
		}
	}
	return nil
}

func (s *Server) get(ref *ateapipb.ObjectRef) (*ateapipb.Actor, error) {
	a, ok := s.actors[key(ref.GetAtespace(), ref.GetName())]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "actor %q not found", ref.GetName())
	}
	return a, nil
}

func clone(a *ateapipb.Actor) *ateapipb.Actor {
	return proto.Clone(a).(*ateapipb.Actor)
}

func (s *Server) GetActor(ctx context.Context, req *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.get(req.GetActor())
	if err != nil {
		return nil, err
	}
	return clone(a), nil
}

func (s *Server) CreateActor(ctx context.Context, req *ateapipb.CreateActorRequest) (*ateapipb.Actor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	actor := req.GetActor()
	name := actor.GetMetadata().GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "metadata.name is required")
	}
	k := key(actor.GetMetadata().GetAtespace(), name)
	if _, ok := s.actors[k]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "actor %q already exists", name)
	}
	if ref := actor.GetActorTemplate(); s.RequireTemplate && s.templates[key(ref.GetAtespace(), ref.GetName())] == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "actor template not found; ObjectRef: %s/%s", ref.GetAtespace(), ref.GetName())
	}
	a := clone(actor)
	a.Status = &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}
	s.actors[k] = a
	return clone(a), nil
}

func (s *Server) ResumeActor(ctx context.Context, req *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.get(req.GetActor())
	if err != nil {
		return nil, err
	}
	a.Status = &ateapipb.ActorStatus{
		State: ateapipb.ActorState_ACTOR_STATE_RUNNING,
		WorkerAssignment: &ateapipb.WorkerAssignment{
			WorkerNamespace: "ate-system",
			WorkerPod:       "worker-0",
			WorkerPodIps:    []string{"10.0.0.1"},
		},
	}
	return &ateapipb.ResumeActorResponse{Actor: clone(a)}, nil
}

func (s *Server) SuspendActor(ctx context.Context, req *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.get(req.GetActor())
	if err != nil {
		return nil, err
	}
	// SuspendModes records each call, also a rejected one.
	s.SuspendModes = append(s.SuspendModes, req.GetOptions().GetMode())
	// Like Substrate v0.6.0: a SUSPENDED actor is a no-op, and only a
	// RUNNING or SUSPENDING one can be suspended.
	switch st := a.GetStatus().GetState(); st {
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
		return &ateapipb.SuspendActorResponse{Actor: clone(a)}, nil
	case ateapipb.ActorState_ACTOR_STATE_RUNNING, ateapipb.ActorState_ACTOR_STATE_SUSPENDING:
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "MarkSuspending prerequisite not met for Actor: %s (got: %v, want %s)",
			req.GetActor().GetName(), st, ateapipb.ActorState_ACTOR_STATE_RUNNING)
	}
	s.suspend(a, req.GetOptions().GetMode())
	return &ateapipb.SuspendActorResponse{Actor: clone(a)}, nil
}

// suspend checkpoints a. The caller holds s.mu. A synchronous suspend leaves
// the snapshot in object storage; any other mode (asynchronous, which is also
// Substrate's default when unset) leaves it on a node with the upload pending,
// as Substrate does until atelet's background upload lands (CompleteUpload).
func (s *Server) suspend(a *ateapipb.Actor, mode ateapipb.DurabilityMode) {
	snap := &ateapipb.Snapshot{
		Uuid:       fmt.Sprintf("snapshot-%s", a.GetMetadata().GetName()),
		Generation: a.GetStatus().GetLastAssignedGeneration() + 1,
		Owner:      ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR,
		Fidelity:   ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
		DurableSnapshot: &ateapipb.SnapshotStorage{
			State:  ateapipb.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED,
			Object: &ateapipb.ObjectSnapshot{},
		},
	}
	if mode != ateapipb.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS {
		snap.Locality = "node-1"
		snap.DurableSnapshot.State = ateapipb.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_PENDING
	}
	a.Status = &ateapipb.ActorStatus{
		State:                  ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
		Snapshots:              []*ateapipb.Snapshot{snap},
		LastAssignedGeneration: snap.GetGeneration(),
	}
}

// CompleteUpload marks the actor's snapshots durable, as atelet's background
// upload after an asynchronous suspend does once it lands.
func (s *Server) CompleteUpload(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, snap := range s.find(name).GetStatus().GetSnapshots() {
		if snap.GetDurableSnapshot() != nil {
			snap.DurableSnapshot.State = ateapipb.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED
		}
	}
}

func (s *Server) DeleteActor(ctx context.Context, req *ateapipb.DeleteActorRequest) (*ateapipb.Actor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := req.GetActor()
	a, err := s.get(ref)
	if err != nil {
		return nil, err
	}
	// Like Substrate v0.6.0, DeleteActor takes an actor in any state: it
	// terminates the workload itself.
	delete(s.actors, key(ref.GetAtespace(), ref.GetName()))
	return clone(a), nil
}

func (s *Server) ListActors(ctx context.Context, req *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	resp := &ateapipb.ListActorsResponse{}
	for _, a := range s.actors {
		if req.GetAtespace() != "" && a.GetMetadata().GetAtespace() != req.GetAtespace() {
			continue
		}
		resp.Actors = append(resp.Actors, clone(a))
	}
	return resp, nil
}

func (s *Server) CreateAtespace(ctx context.Context, req *ateapipb.CreateAtespaceRequest) (*ateapipb.Atespace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	space := req.GetAtespace()
	name := space.GetMetadata().GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "metadata.name is required")
	}
	if s.DenyCreateAtespace {
		return nil, status.Errorf(codes.PermissionDenied, "not allowed to create atespace %q", name)
	}
	if _, ok := s.atespaces[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "atespace %q already exists", name)
	}
	a := proto.Clone(space).(*ateapipb.Atespace)
	s.atespaces[name] = a
	return proto.Clone(a).(*ateapipb.Atespace), nil
}

func (s *Server) GetAtespace(ctx context.Context, req *ateapipb.GetAtespaceRequest) (*ateapipb.Atespace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := req.GetAtespace().GetName()
	a, ok := s.atespaces[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "atespace %q not found", name)
	}
	return proto.Clone(a).(*ateapipb.Atespace), nil
}

# Generated protobuf and gRPC code

`env.pb.go`, `env_grpc.pb.go`, `guest.pb.go`, and `guest_grpc.pb.go` are generated code. Never
hand-edit these files.

To update Go stubs after modifying `*.proto`:

```bash
buf generate
```

To update Python stubs, run `make python-protos` from the repository root. See
[docs/dev/code-layout.md](../../../docs/dev/code-layout.md).

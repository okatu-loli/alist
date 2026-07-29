SWAG := $(shell go env GOPATH)/bin/swag
BUF_VERSION := v1.71.0

.PHONY: swagger
swagger:
	$(SWAG) init -g main.go -o docs --parseDependency --parseInternal --parseDepth 3

# proto regenerates the gRPC plugin stubs in ext/grpcproto from driver.proto.
# buf bundles its own protobuf compiler (no system protoc needed); the codegen
# plugins run via `go run ...@version` so nothing has to be installed on PATH.
.PHONY: proto
proto:
	go run github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION) generate

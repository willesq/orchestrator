# Claude Orchestrator (Go) — build recipes.
#
# The ORT embedder needs cgo + a tokenizers static lib at link time and the
# onnxruntime shared lib at runtime (Homebrew). See docs/GO_REWRITE_PLAN.md.

GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
PLATFORM := $(GOOS)-$(GOARCH)

# Where the prebuilt libtokenizers.a lives (daulet/tokenizers release asset).
TOK_LIB_DIR := build/lib/$(PLATFORM)
ONNX_DIR    ?= $(shell brew --prefix onnxruntime 2>/dev/null)/lib

BIN_DIR := build/bin
TAGS    := ORT
CGO_LDFLAGS := -L$(TOK_LIB_DIR)

export CGO_ENABLED = 1

.PHONY: build orch orchd orch-hook test cover vet fmt clean tools proto

# Pinned to the versions stamped in the committed *.pb.go headers so regenerating
# produces a diff of the schema change only.
PROTOC_GEN_GO      := google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
PROTOC_GEN_GO_GRPC := google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
PROTO              := internal/rpc/orchdv1/orchd.proto

build: orch orchd orch-hook

orch:
	CGO_LDFLAGS="$(CGO_LDFLAGS)" go build -tags $(TAGS) -o $(BIN_DIR)/orch      ./cmd/orch

orchd:
	CGO_LDFLAGS="$(CGO_LDFLAGS)" go build -tags $(TAGS) -o $(BIN_DIR)/orchd     ./cmd/orchd

orch-hook:
	CGO_LDFLAGS="$(CGO_LDFLAGS)" go build -tags $(TAGS) -o $(BIN_DIR)/orch-hook ./cmd/orch-hook

# Tests / vet build WITHOUT the ORT tag (hugot disabled stub) so cgo/native libs
# aren't required in plain CI; embedder integration is exercised separately.
test:
	go test ./...

# Per-file coverage gate (>= 80% on every non-generated, non-entrypoint file).
cover:
	@bash scripts/cover.sh 80

vet:
	go vet ./...

# Install the codegen plugins protoc shells out to (into $(go env GOPATH)/bin).
tools:
	go install $(PROTOC_GEN_GO)
	go install $(PROTOC_GEN_GO_GRPC)

# Regenerate the orchd gRPC stubs. Run after editing the .proto; needs `make tools`
# and protoc (brew install protobuf) once.
proto: tools
	PATH="$$(go env GOPATH)/bin:$$PATH" protoc \
	  --go_out=. --go_opt=module=github.com/willesq/orchestrator \
	  --go-grpc_out=. --go-grpc_opt=module=github.com/willesq/orchestrator \
	  $(PROTO)

fmt:
	gofmt -w cmd internal

clean:
	rm -rf $(BIN_DIR)

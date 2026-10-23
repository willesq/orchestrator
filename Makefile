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

.PHONY: build orch orchd orch-hook test cover vet fmt clean tools proto proto-go proto-swift app app-install app-test app-cover

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

# Regenerate the orchd gRPC stubs (Go for the daemon and CLI, Swift for the
# macOS app). Run after editing the .proto; needs `make tools`, protoc
# (brew install protobuf) and Xcode once.
proto: proto-go proto-swift

proto-go: tools
	PATH="$$(go env GOPATH)/bin:$$PATH" protoc \
	  --go_out=. --go_opt=module=github.com/willesq/orchestrator \
	  --go-grpc_out=. --go-grpc_opt=module=github.com/willesq/orchestrator \
	  $(PROTO)

# The Swift plugins are built from the versions pinned in macos/Package.resolved,
# so the generated code always matches the runtime the app links.
SWIFT_PKG     := macos
SWIFT_GEN_DIR := $(SWIFT_PKG)/Sources/OrchKit/Generated
SWIFT_BIN      = $(shell swift build -c release --package-path $(SWIFT_PKG) --show-bin-path)

proto-swift:
	swift build -c release --package-path $(SWIFT_PKG) --only-use-versions-from-resolved-file --product protoc-gen-swift
	swift build -c release --package-path $(SWIFT_PKG) --only-use-versions-from-resolved-file --product protoc-gen-grpc-swift-2
	mkdir -p $(SWIFT_GEN_DIR)
	protoc -I internal/rpc/orchdv1 \
	  --plugin=protoc-gen-swift=$(SWIFT_BIN)/protoc-gen-swift \
	  --plugin=protoc-gen-grpc-swift-2=$(SWIFT_BIN)/protoc-gen-grpc-swift-2 \
	  --swift_out=$(SWIFT_GEN_DIR) --swift_opt=Visibility=Public \
	  --grpc-swift-2_out=$(SWIFT_GEN_DIR) \
	  --grpc-swift-2_opt=Visibility=Public,Server=false,Client=true \
	  $(PROTO)

# The macOS app: Orch.app assembled under build/app.noindex (ad-hoc signed unless
# SIGN_IDENTITY names a Developer ID certificate).
app:
	bash $(SWIFT_PKG)/scripts/build-app.sh

# Builds Orch.app and installs it in ~/Applications (INSTALL_DIR overrides),
# replacing and relaunching an earlier copy.
app-install:
	bash $(SWIFT_PKG)/scripts/install-app.sh

app-test:
	swift test --package-path $(SWIFT_PKG)

app-cover:
	bash $(SWIFT_PKG)/scripts/cover.sh 80

fmt:
	gofmt -w cmd internal

clean:
	rm -rf $(BIN_DIR)

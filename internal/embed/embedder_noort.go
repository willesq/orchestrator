//go:build !ORT

package embed

import "errors"

// defaultLoader errors when the binary was built without the ORT backend, so a
// plain `go build`/`go test` (no cgo, no native libs) still compiles and runs.
func defaultLoader(_ Config) (runner, int, error) {
	return nil, 0, errors.New("embedder unavailable: built without -tags ORT")
}

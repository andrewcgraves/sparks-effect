package valhalla

import (
	"context"
	"sync"
)

// StubClient is a recording double for tests: it answers with whatever it was
// given and remembers what it was asked.
//
// It is distinct from SyntheticClient, which computes plausible answers and is
// selected in production when no server is configured. This one computes
// nothing — which is what makes it useful for asserting *what the chainer
// asked for*, the thing most of the migrated chainer suite is actually about.
//
// It lives in the package rather than in a _test.go file because the chainer's
// tests are in another package and cannot reach a test-only type here.
type StubClient struct {
	mu sync.Mutex

	IsochroneResp *IsochroneResponse
	IsochroneErr  error
	MatrixResp    *MatrixResponse
	MatrixErr     error
	StatusResp    Status
	StatusErr     error

	// IsochroneFn, when set, takes precedence over IsochroneResp/IsochroneErr.
	// It is how a test makes one station in a fan-out fail while the rest
	// succeed, which a single canned response cannot express.
	IsochroneFn func(req IsochroneRequest) (*IsochroneResponse, error)

	IsochroneCalls []IsochroneRequest
	MatrixCalls    []MatrixRequest
}

var _ Client = (*StubClient)(nil)

func (s *StubClient) Isochrone(_ context.Context, req IsochroneRequest) (*IsochroneResponse, error) {
	s.mu.Lock()
	s.IsochroneCalls = append(s.IsochroneCalls, req)
	fn := s.IsochroneFn
	s.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return s.IsochroneResp, s.IsochroneErr
}

func (s *StubClient) Matrix(_ context.Context, req MatrixRequest) (*MatrixResponse, error) {
	s.mu.Lock()
	s.MatrixCalls = append(s.MatrixCalls, req)
	s.mu.Unlock()
	return s.MatrixResp, s.MatrixErr
}

func (s *StubClient) Status(context.Context) (Status, error) {
	return s.StatusResp, s.StatusErr
}

// Calls returns copies of what the stub was asked, safe to read while a chain
// that is still running holds the stub.
func (s *StubClient) Calls() (isochrone []IsochroneRequest, matrix []MatrixRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	isochrone = append(isochrone, s.IsochroneCalls...)
	matrix = append(matrix, s.MatrixCalls...)
	return isochrone, matrix
}

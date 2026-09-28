package valhalla

import (
	"context"
	"sync"
)

type StubClient struct {
	mu             sync.Mutex
	IsochroneResp  *IsochroneResponse
	IsochroneErr   error
	MatrixResp     *MatrixResponse
	MatrixErr      error
	RouteResp      *RouteResponse
	RouteErr       error
	StatusResp     Status
	StatusErr      error
	IsochroneFn    func(req IsochroneRequest) (*IsochroneResponse, error)
	IsochroneCtxFn func(ctx context.Context, req IsochroneRequest) (*IsochroneResponse, error)
	RouteFn        func(req RouteRequest) (*RouteResponse, error)
	isochroneCalls []IsochroneRequest
	matrixCalls    []MatrixRequest
	routeCalls     []RouteRequest
}

var _ Client = (*StubClient)(nil)

func (s *StubClient) Isochrone(ctx context.Context, req IsochroneRequest) (*IsochroneResponse, error) {
	s.mu.Lock()
	s.isochroneCalls = append(s.isochroneCalls, req)
	ctxFn := s.IsochroneCtxFn
	fn := s.IsochroneFn
	s.mu.Unlock()
	if ctxFn != nil {
		return ctxFn(ctx, req)
	}
	if fn != nil {
		return fn(req)
	}
	return s.IsochroneResp, s.IsochroneErr
}

func (s *StubClient) Matrix(_ context.Context, req MatrixRequest) (*MatrixResponse, error) {
	s.mu.Lock()
	s.matrixCalls = append(s.matrixCalls, req)
	s.mu.Unlock()
	return s.MatrixResp, s.MatrixErr
}

func (s *StubClient) Route(_ context.Context, req RouteRequest) (*RouteResponse, error) {
	s.mu.Lock()
	s.routeCalls = append(s.routeCalls, req)
	fn := s.RouteFn
	s.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return s.RouteResp, s.RouteErr
}

func (s *StubClient) Status(context.Context) (Status, error) {
	return s.StatusResp, s.StatusErr
}

func (s *StubClient) Calls() (isochrone []IsochroneRequest, matrix []MatrixRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	isochrone = append(isochrone, s.isochroneCalls...)
	matrix = append(matrix, s.matrixCalls...)
	return isochrone, matrix
}

func (s *StubClient) RouteCalls() []RouteRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RouteRequest(nil), s.routeCalls...)
}

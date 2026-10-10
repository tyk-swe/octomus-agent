package httpapi

import "testing"

func TestAPIRouteInventory(t *testing.T) {
	want := []struct {
		method  string
		pattern string
	}{
		{"GET", "/api/state"},
		{"GET", "/api/tasks"},
		{"GET", "/api/cycles"},
		{"GET", "/api/cycles/{id}"},
		{"GET", "/api/cycles/{id}/evidence"},
		{"POST", "/api/cycles/{id}/{action}"},
		{"GET", "/api/proposals"},
		{"GET", "/api/proposals/{cycle}/{id}"},
		{"GET", "/api/prs"},
		{"GET", "/api/tasks/{id}"},
		{"POST", "/api/tasks/{id}/{action}"},
		{"GET", "/api/config"},
		{"PUT", "/api/config"},
		{"POST", "/api/baseline-checks"},
		{"GET", "/api/baseline-checks/latest"},
		{"GET", "/api/baseline-checks/{id}"},
		{"POST", "/api/baseline-checks/{id}/cancel"},
		{"POST", "/api/control/{action}"},
		{"POST", "/api/doctor"},
		{"POST", "/api/sandbox/self-test"},
		{"POST", "/api/model-catalog"},
		{"GET", "/api/events"},
	}
	if len(apiRoutes) != len(want) {
		t.Fatalf("route count = %d, want %d", len(apiRoutes), len(want))
	}
	for i, route := range apiRoutes {
		t.Run(want[i].method+" "+want[i].pattern, func(t *testing.T) {
			if route.method != want[i].method || route.pattern != want[i].pattern {
				t.Fatalf("route = %s %s, want %s %s", route.method, route.pattern, want[i].method, want[i].pattern)
			}
			if route.handler == nil {
				t.Fatal("route handler is nil")
			}
		})
	}
}

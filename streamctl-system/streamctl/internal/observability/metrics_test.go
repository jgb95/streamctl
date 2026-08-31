package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsAuthentication(t *testing.T) {
	m := New("test_auth")
	h := m.Handler("secret")

	for _, test := range []struct {
		name   string
		token  string
		status int
	}{
		{name: "missing", status: http.StatusUnauthorized},
		{name: "wrong", token: "wrong", status: http.StatusUnauthorized},
		{name: "valid", token: "secret", status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if test.token != "" {
				req.Header.Set("Authorization", "Bearer "+test.token)
			}
			res := httptest.NewRecorder()
			h.ServeHTTP(res, req)
			if res.Code != test.status {
				t.Fatalf("status = %d, want %d", res.Code, test.status)
			}
		})
	}
}

func TestDisabledMetricsAreNotFound(t *testing.T) {
	res := httptest.NewRecorder()
	New("test_disabled").Handler("").ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNotFound)
	}
}

func TestMiddlewareUsesServeMuxPattern(t *testing.T) {
	m := New("test_routes")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /streams/edit/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	res := httptest.NewRecorder()
	m.Middleware(mux).ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/streams/edit/private-stream-id", nil))
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusCreated)
	}

	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, family := range families {
		text += family.String()
	}
	if !strings.Contains(text, `name:"route" value:"POST /streams/edit/{id}"`) {
		t.Fatalf("route pattern missing from metrics: %s", text)
	}
	if strings.Contains(text, "private-stream-id") {
		t.Fatalf("raw path leaked into metric labels: %s", text)
	}
}

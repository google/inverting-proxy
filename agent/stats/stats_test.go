package stats

import (
	"expvar"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDebugVars(t *testing.T) {
	// The debug server is global, so we don't need to start it.
	// We just need to make a request to the /debug/vars endpoint.
	req, err := http.NewRequest("GET", "/debug/vars", nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	http.DefaultServeMux.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusOK)
	}
}

func TestServeStats(t *testing.T) {
	// Use existing response_codes expvar
	if responseCodes := expvar.Get("response_codes"); responseCodes != nil {
		if rcMap, ok := responseCodes.(*expvar.Map); ok {
			rcMap.Add("200", 10)
			rcMap.Add("404", 2)
		}
	}

	// Use existing response_times expvar
	if responseTimes := expvar.Get("response_times"); responseTimes != nil {
		if rtMap, ok := responseTimes.(*expvar.Map); ok {
			if p50 := rtMap.Get("p50"); p50 != nil {
				p50.(*expvar.Float).Set(12.5)
			}
			if p90 := rtMap.Get("p90"); p90 != nil {
				p90.(*expvar.Float).Set(25.8)
			}
			if p99 := rtMap.Get("p99"); p99 != nil {
				p99.(*expvar.Float).Set(50.3)
			}
		}
	}

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()

	serveStats(rr, req, "testBackend", "http://test-proxy:8080")

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("serveStats returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	body := rr.Body.String()
	expectedStrings := []string{
		"testBackend",
		"http://test-proxy:8080",
	}

	for _, expected := range expectedStrings {
		if !strings.Contains(body, expected) {
			t.Errorf("serveStats output missing expected string %q", expected)
		}
	}
}

func TestServeStatsWithEmptyExpvar(t *testing.T) {
	// Ensure the handler doesn't panic with nil/empty expvar values
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()

	// This should not panic even if expvar values don't exist
	serveStats(rr, req, "testBackend", "http://test-proxy:8080")

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("serveStats returned wrong status code with empty expvar: got %v want %v", status, http.StatusOK)
	}

	body := rr.Body.String()
	if !strings.Contains(body, "testBackend") {
		t.Error("serveStats output missing backend ID with empty expvar")
	}
}

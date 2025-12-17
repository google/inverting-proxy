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
	// Set up test expvar data
	testResponseCodes := expvar.NewMap("test_response_codes")
	testResponseCodes.Add("200", 10)
	testResponseCodes.Add("404", 2)

	testResponseTimes := expvar.NewMap("test_response_times")
	testResponseTimes.Set("p50", new(expvar.Float))
	testResponseTimes.Set("p90", new(expvar.Float))
	testResponseTimes.Set("p99", new(expvar.Float))
	testResponseTimes.Get("p50").(*expvar.Float).Set(12.5)
	testResponseTimes.Get("p90").(*expvar.Float).Set(25.8)
	testResponseTimes.Get("p99").(*expvar.Float).Set(50.3)

	// Temporarily replace global expvar values
	oldResponseCodes := expvar.Get("response_codes")
	oldResponseTimes := expvar.Get("response_times")
	expvar.Publish("response_codes", testResponseCodes)
	expvar.Publish("response_times", testResponseTimes)
	defer func() {
		if oldResponseCodes != nil {
			expvar.Publish("response_codes", oldResponseCodes)
		}
		if oldResponseTimes != nil {
			expvar.Publish("response_times", oldResponseTimes)
		}
	}()

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
		"200",
		"404",
		"12.5",
		"25.8",
		"50.3",
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

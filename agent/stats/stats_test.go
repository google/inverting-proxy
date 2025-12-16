package stats

import (
	_ "expvar"
	"net/http"
	"net/http/httptest"
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

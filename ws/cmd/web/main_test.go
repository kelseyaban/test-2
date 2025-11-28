// Filename: cmd/web/main_test.go

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerWS(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	rr := httptest.NewRecorder()
	handlerHome(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v expected %v", status, http.StatusOK)
	}
	
	expected := "WebSockets!\n"
	if got := rr.Body.String(); got != expected {
		t.Errorf("handler returned unexpected body: got %q expected %q", got, expected)
	}
}

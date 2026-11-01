package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaintenanceConsoleShowsStatusWithoutOpeningDatabase(t *testing.T) {
	root := t.TempDir()
	problem := errors.New("reset incomplete: locked custom.db")
	h := maintenanceHandler(root, nil)
	for _, route := range []string{"/", "/status", "/doctor"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+route, nil))
		var state map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		if state["maintenance"] != true || state["bot_running"] != false || state["schedulers_running"] != false || state["error"] == problem.Error() || state["recovery_commands"] == nil {
			t.Fatalf("%s: %v", route, state)
		}
	}
	for _, id := range []string{"runtime.doctor", "runtime.status", "setup.resume"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "http://localhost/api/action", strings.NewReader(`{"action":"`+id+`"}`)))
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if (id == "setup.resume") == (result["ok"] == true) {
			t.Fatalf("%s: %v", id, result)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "masamong.db")); !os.IsNotExist(err) {
		t.Fatal("maintenance console created a live DB")
	}
}

func TestMaintenanceConsoleRejectsRebindingAndHidesErrorSecrets(t *testing.T) {
	h := maintenanceHandler(t.TempDir(), nil)
	for _, host := range []string{"attacker.example", "localhost.attacker.example", "127.0.0.1.attacker.example"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "http://localhost/", nil)
		r.Host = host
		h.ServeHTTP(w, r)
		if w.Code != http.StatusMisdirectedRequest || strings.Contains(w.Body.String(), "synthetic-secret") {
			t.Fatalf("%s: %d %s", host, w.Code, w.Body.String())
		}
	}
	for _, host := range []string{"localhost:8787", "127.0.0.2:8787", "[::1]:8787"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "http://localhost/", nil)
		r.Host = host
		h.ServeHTTP(w, r)
		if w.Code != 200 || strings.Contains(w.Body.String(), "synthetic-secret") {
			t.Fatalf("%s: %d %s", host, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "http://localhost/api/action", strings.NewReader(`{"action":"runtime.doctor"}`))
	r.Header.Set("Origin", "https://attacker.example")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
}

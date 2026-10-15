package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTarget(t *testing.T) {
	ctx := context.Background()
	addr, hash, err := Config{URL: "https://node1.example.com:4443/moq", CertHash: "ab"}.Target(ctx, "tok")
	if err != nil || addr != "node1.example.com:4443" || hash != "ab" {
		t.Errorf("fixed URL: %q %q %v", addr, hash, err)
	}

	var auth string
	route := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if auth != "Bearer good" {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"url":"https://node2:4444/moq","certHash":"cd"}`))
	}))
	defer route.Close()
	cfg := Config{RouteURL: route.URL, URL: "https://ignored:1/moq"}
	addr, hash, err = cfg.Target(ctx, "good")
	if err != nil || addr != "node2:4444" || hash != "cd" || auth != "Bearer good" {
		t.Errorf("routed: %q %q %v (auth %q)", addr, hash, err, auth)
	}
	if _, _, err := cfg.Target(ctx, "bad"); err == nil {
		t.Error("route refusal ignored")
	}
	if _, _, err := (Config{URL: "node1:4443"}).Target(ctx, "tok"); err == nil {
		t.Error("URL without a host accepted")
	}
}

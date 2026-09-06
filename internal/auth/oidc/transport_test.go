package oidc

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestBoundedClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		_, _ = w.Write(bytes.Repeat([]byte("x"), n))
	}))
	t.Cleanup(srv.Close)

	base := srv.Client()
	c := boundedClient(base)
	if base.Timeout != 0 || base.Transport == c.Transport {
		t.Fatal("base client was modified")
	}
	if c.Timeout != DefaultHTTPTimeout {
		t.Fatalf("default timeout not applied: %v", c.Timeout)
	}
	if kept := boundedClient(&http.Client{Timeout: time.Second}); kept.Timeout != time.Second {
		t.Fatalf("explicit timeout overridden: %v", kept.Timeout)
	}

	read := func(n int) (int, error) {
		resp, err := c.Get(srv.URL + "?n=" + strconv.Itoa(n))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		return len(body), err
	}
	if n, err := read(MaxDocumentBytes); err != nil || n != MaxDocumentBytes {
		t.Fatalf("document at the limit: %d bytes, %v", n, err)
	}
	if _, err := read(MaxDocumentBytes + 1); !errors.Is(err, errDocumentTooLarge) {
		t.Fatalf("document over the limit: %v", err)
	}
	if n, err := read(0); err != nil || n != 0 {
		t.Fatalf("empty document: %d, %v", n, err)
	}
}

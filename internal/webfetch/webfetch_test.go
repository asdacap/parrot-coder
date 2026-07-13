package webfetch

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestLocalhostOptInHTMLAndReview(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<html><style>secret</style><body><h1>Hello &amp; bye</h1><script>alert(1)</script><p>Text`+"\x01"+`</p></body></html>`)
	}))
	defer server.Close()
	request := Request{URL: server.URL, Method: "get"}
	if _, err := New(Config{}).Fetch(context.Background(), request); err == nil {
		t.Fatal("localhost accepted without opt-in")
	}
	response, err := New(Config{AllowPrivate: true}).Fetch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != 200 || response.ContentType != "text/html" || strings.Contains(response.Text, "secret") || strings.Contains(response.Text, "alert") || strings.ContainsRune(response.Text, '\x01') {
		t.Fatalf("response = %#v", response)
	}
	if review := Review(request); review.URL != server.URL || review.Method != "GET" {
		t.Fatalf("review = %#v", review)
	}
}

func TestRedirectRevalidationAndSensitiveHeaderStripping(t *testing.T) {
	var authorization, cookie string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization, cookie = r.Header.Get("Authorization"), r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()
	response, err := New(Config{AllowPrivate: true}).Fetch(context.Background(), Request{
		URL: source.URL, Headers: http.Header{"Authorization": {"Bearer secret"}, "Cookie": {"a=b"}},
	})
	if err != nil || response.Text != "ok" {
		t.Fatalf("redirect response = %#v, %v", response, err)
	}
	if authorization != "" || cookie != "" {
		t.Fatalf("sensitive redirect headers leaked: %q %q", authorization, cookie)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "file:///etc/passwd")
		w.WriteHeader(http.StatusFound)
	}))
	defer bad.Close()
	if _, err := New(Config{AllowPrivate: true}).Fetch(context.Background(), Request{URL: bad.URL}); err == nil {
		t.Fatal("forbidden redirect scheme accepted")
	}
}

func TestBoundedBodyGzipAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		_, _ = io.WriteString(writer, strings.Repeat("a", 10_000))
		_ = writer.Close()
	}))
	defer server.Close()
	response, err := New(Config{AllowPrivate: true, MaxBodyBytes: 64}).Fetch(context.Background(), Request{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Truncated || len(response.Text) != 64 || response.Text != strings.Repeat("a", 64) {
		t.Fatalf("bounded gzip response = %#v", response)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = New(Config{AllowPrivate: true, Timeout: time.Second}).Fetch(ctx, Request{URL: slow.URL})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestSanitizeMalformedHTML(t *testing.T) {
	got := SanitizeHTML(`<h1>Title</h1><!-- comment > still comment --><p>A&nbsp; B</p><script>bad</script><div title=">">C</div>`)
	if strings.Contains(got, "comment") || strings.Contains(got, "bad") || got != "Title\n\nA B\n\nC" {
		t.Fatalf("sanitized HTML = %q", got)
	}
}

func TestValidateIPRejectsSpecialUseNetworks(t *testing.T) {
	for _, raw := range []string{"0.0.0.1", "100.64.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "240.0.0.1", "64:ff9b:1::1", "100::1", "2001:db8::1"} {
		if err := validateIP(netip.MustParseAddr(raw), false); err == nil {
			t.Errorf("special-use address %s accepted", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if err := validateIP(netip.MustParseAddr(raw), false); err != nil {
			t.Errorf("public address %s rejected: %v", raw, err)
		}
	}
}

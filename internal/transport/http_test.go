package transport

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// newHTTPServer starts a listener with HTTP enabled and a tiny HTTP server
// on its HTTP side. It returns the tc pin and the HTTP pin.
func newHTTPServer(t *testing.T) (ln Listener, tcPin, httpPin string) {
	t.Helper()
	dir := t.TempDir()
	hc, err := LoadOrCreateHTTPCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	ln, cert := newServer(t, listenOpts{ListenOpts: ListenOpts{HTTP: true, HTTPCert: hc}})
	hl := HTTPListener(ln)
	if hl == nil {
		t.Fatal("no HTTP listener")
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "hi "+r.Method+" "+r.URL.Path)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go srv.Serve(hl)
	t.Cleanup(func() { srv.Close() })
	return ln, Pin(cert), Pin(hc)
}

func httpsClient(pin string) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: HTTPSConfig(pin)}}
}

func TestHTTPRouting(t *testing.T) {
	ln, tcPin, httpPin := newHTTPServer(t)
	if tcPin == httpPin {
		t.Fatal("HTTP cert is the tc cert")
	}

	resp, err := httpsClient(httpPin).Get("https://" + ln.Addr().String() + "/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hi GET /x" || resp.TLS.NegotiatedProtocol != ALPNHTTP {
		t.Fatalf("got %q over %q", body, resp.TLS.NegotiatedProtocol)
	}
	// HTTP clients never see the tc key.
	if _, err := httpsClient(tcPin).Get("https://" + ln.Addr().String() + "/x"); !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("HTTP side with tc pin: %v", err)
	}

	// tc still works on both transports of the same listener, with the tc cert.
	for _, k := range kinds {
		c, _ := connect(t, ln, tcPin, k.dial)
		if c.Kind() != k.name {
			t.Fatalf("Kind = %s", c.Kind())
		}
	}

	// No ALPN at all is an HTTP client (a tc client always offers tc/1).
	conf := HTTPSConfig(httpPin)
	conf.NextProtos = nil
	tc, err := tls.Dial("tcp", ln.Addr().String(), conf)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	io.WriteString(tc, "GET /plain HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	raw, _ := io.ReadAll(tc)
	if !strings.Contains(string(raw), "hi GET /plain") {
		t.Fatalf("no-ALPN response: %q", raw)
	}
}

func TestHTTPRejectsOtherALPN(t *testing.T) {
	ln, _, httpPin := newHTTPServer(t)
	conf := HTTPSConfig(httpPin)
	conf.NextProtos = []string{"h2", "foo"}
	c, err := tls.Dial("tcp", ln.Addr().String(), conf)
	if err == nil {
		c.Close()
		t.Fatal("handshake succeeded offering neither tc/1 nor http/1.1")
	}
}

func TestHTTPDisabledByDefault(t *testing.T) {
	ln, cert := newServer(t, listenOpts{})
	if HTTPListener(ln) != nil {
		t.Fatal("HTTP listener without ListenOpts.HTTP")
	}
	_, err := httpsClient(Pin(cert)).Get("https://" + ln.Addr().String() + "/")
	if err == nil {
		t.Fatal("HTTP request succeeded on a tc-only listener")
	}
	// The tc client still offers only tc/1.
	if got := clientTLS("x").NextProtos; len(got) != 1 || got[0] != "tc/1" {
		t.Fatalf("client ALPN %v", got)
	}
}

func TestHTTPListenerClose(t *testing.T) {
	ln, _ := newServer(t, listenOpts{ListenOpts: ListenOpts{HTTP: true}})
	hl := HTTPListener(ln)
	errc := make(chan error, 1)
	go func() { _, err := hl.Accept(); errc <- err }()
	ln.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP Accept not unblocked by Close")
	}
}

func TestHTTPCertPersisted(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateHTTPCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateHTTPCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := LoadOrCreateCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	if Pin(a) == "" || Pin(a) != Pin(b) || Pin(a) == Pin(c) {
		t.Fatalf("pins: %q %q %q", Pin(a), Pin(b), Pin(c))
	}
}

func TestCurlPin(t *testing.T) {
	cert, err := LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := CurlPin(Pin(cert))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(cert.Leaf.RawSubjectPublicKeyInfo)
	if want := "sha256//" + base64.StdEncoding.EncodeToString(sum[:]); got != want {
		t.Fatalf("CurlPin = %q, want %q", got, want)
	}
	if _, err := CurlPin("not a pin"); err == nil {
		t.Fatal("bad pin accepted")
	}
}

// TestCurl checks the exact invocation the docs and hook.create give out.
func TestCurl(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not in PATH")
	}
	ln, tcPin, httpPin := newHTTPServer(t)
	cp, _ := CurlPin(httpPin)
	url := "https://" + ln.Addr().String() + "/hooks/abc"
	out, err := exec.Command(curl, "-sS", "-k", "--pinnedpubkey", cp, "-d", "x", url).CombinedOutput()
	if err != nil {
		t.Fatalf("curl: %v: %s", err, out)
	}
	if string(out) != "hi POST /hooks/abc" {
		t.Fatalf("curl got %q", out)
	}
	// A wrong pin must fail even with -k.
	bad, _ := CurlPin(tcPin)
	if out, err := exec.Command(curl, "-sS", "-k", "--pinnedpubkey", bad, "-d", "x", url).CombinedOutput(); err == nil {
		t.Fatalf("curl with wrong pin succeeded: %s", out)
	}
}

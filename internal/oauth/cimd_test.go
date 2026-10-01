package oauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fedorov-xyz/ah-mcp/internal/util"
)

type cimdServer struct {
	*httptest.Server
	hits atomic.Int64
	doc  func(self string) any
}

func newCIMDServer(t *testing.T, doc func(self string) any) *cimdServer {
	t.Helper()
	cs := &cimdServer{doc: doc}
	cs.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.hits.Add(1)
		self := "https://" + r.Host + r.URL.Path
		switch v := cs.doc(self).(type) {
		case string:
			w.Header().Set("Location", v)
			w.WriteHeader(http.StatusFound)
		case []byte:
			_, _ = w.Write(v)
		default:
			_ = json.NewEncoder(w).Encode(v)
		}
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *cimdServer) tls() *tls.Config {
	return cs.Client().Transport.(*http.Transport).TLSClientConfig
}

func allowAll(netip.Addr) bool { return true }

func goodDoc(self string) any {
	return map[string]any{"client_id": self, "client_name": "CIMD Client", "redirect_uris": []string{cb}, "token_endpoint_auth_method": "none"}
}

func TestCIMDBlocksPrivateAddresses(t *testing.T) {
	cs := newCIMDServer(t, goodDoc)
	// Production address policy; the test server listens on 127.0.0.1.
	f := NewCIMDFetcherForTest(util.IsPublicAddr, cs.tls())
	_, err := f.Fetch(context.Background(), cs.URL+"/client.json")
	if err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("loopback fetch allowed: %v", err)
	}
	if cs.hits.Load() != 0 {
		t.Fatal("request reached the private address")
	}
}

func TestCIMDValidation(t *testing.T) {
	for name, doc := range map[string]func(string) any{
		"id mismatch": func(string) any {
			return map[string]any{"client_id": "https://other.example/c.json", "client_name": "x", "redirect_uris": []string{cb}}
		},
		"no name":      func(s string) any { return map[string]any{"client_id": s, "redirect_uris": []string{cb}} },
		"no redirects": func(s string) any { return map[string]any{"client_id": s, "client_name": "x"} },
		"bad redirect": func(s string) any {
			return map[string]any{"client_id": s, "client_name": "x", "redirect_uris": []string{"http://evil.example/cb"}}
		},
		"secret client": func(s string) any {
			return map[string]any{"client_id": s, "client_name": "x", "redirect_uris": []string{cb}, "token_endpoint_auth_method": "private_key_jwt"}
		},
		"not json":  func(string) any { return []byte("<html>") },
		"too large": func(string) any { return []byte(`{"client_name":"` + strings.Repeat("a", 70<<10) + `"}`) },
	} {
		cs := newCIMDServer(t, doc)
		f := NewCIMDFetcherForTest(allowAll, cs.tls())
		if _, err := f.Fetch(context.Background(), cs.URL+"/client.json"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	f := NewCIMDFetcherForTest(allowAll, nil)
	for _, id := range []string{"http://example.com/c.json", "https://example.com", "https://example.com/", "https://u:p@example.com/c"} {
		if _, err := f.Fetch(context.Background(), id); err == nil {
			t.Errorf("client_id %q accepted", id)
		}
	}
}

func TestCIMDRedirectToOtherHostDenied(t *testing.T) {
	target := newCIMDServer(t, goodDoc)
	cs := newCIMDServer(t, func(string) any { return target.URL + "/client.json" })
	f := NewCIMDFetcherForTest(allowAll, cs.tls())
	if _, err := f.Fetch(context.Background(), cs.URL+"/client.json"); err == nil || !strings.Contains(err.Error(), "another host") {
		t.Fatalf("cross-host redirect followed: %v", err)
	}
	if target.hits.Load() != 0 {
		t.Fatal("redirect target was contacted")
	}
}

func TestCIMDSuccessAndCache(t *testing.T) {
	cs := newCIMDServer(t, goodDoc)
	f := NewCIMDFetcherForTest(allowAll, cs.tls())
	id := cs.URL + "/client.json"
	for range 3 {
		doc, err := f.Fetch(context.Background(), id)
		if err != nil || doc.ClientName != "CIMD Client" {
			t.Fatalf("fetch: %+v %v", doc, err)
		}
	}
	if cs.hits.Load() != 1 {
		t.Fatalf("document fetched %d times, want cached", cs.hits.Load())
	}

	// Full authorization with a CIMD client id.
	e := newEnv(t, func(o *Options) { o.CIMD = f })
	rec := e.do("GET", authorizeURL(id, cb, nil), "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "CIMD Client") {
		t.Fatalf("authorize with CIMD client: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", authorizeURL(id, "http://127.0.0.1:1/other", nil), ""); rec.Code != 400 {
		t.Fatalf("redirect not in metadata accepted: %d", rec.Code)
	}
	code := e.login(id, cb)
	if status, out := e.token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {id},
		"redirect_uri": {cb}, "code_verifier": {verifier}}); status != 200 {
		t.Fatalf("token for CIMD client: %d %v", status, out)
	}
}

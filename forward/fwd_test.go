package forward

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vulcand/oxy/internal/holsterv4/clock"
	"github.com/vulcand/oxy/testutils"
	"github.com/vulcand/oxy/utils"
)

// Makes sure hop-by-hop headers are removed.
func TestForwardHopHeaders(t *testing.T) {
	called := false
	var outHeaders http.Header
	var outHost, expectedHost string
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		called = true
		outHeaders = req.Header
		outHost = req.Host
		_, _ = w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New()
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		expectedHost = req.URL.Host
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	headers := http.Header{
		Connection: []string{"close"},
		KeepAlive:  []string{"timeout=600"},
	}

	re, body, err := testutils.Get(proxy.URL, testutils.Headers(headers))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(body))
	assert.Equal(t, http.StatusOK, re.StatusCode)
	assert.Equal(t, true, called)
	assert.Equal(t, "", outHeaders.Get(Connection))
	assert.Equal(t, "", outHeaders.Get(KeepAlive))
	assert.Equal(t, expectedHost, outHost)
}

func TestDefaultErrHandler(t *testing.T) {
	f, err := New()
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI("http://localhost:63450")
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	re, _, err := testutils.Get(proxy.URL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadGateway, re.StatusCode)
}

func TestCustomErrHandler(t *testing.T) {
	f, err := New(ErrorHandler(utils.ErrorHandlerFunc(func(w http.ResponseWriter, req *http.Request, err error) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(http.StatusText(http.StatusTeapot)))
	})))
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI("http://localhost:63450")
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	re, body, err := testutils.Get(proxy.URL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusTeapot, re.StatusCode)
	assert.Equal(t, http.StatusText(http.StatusTeapot), string(body))
}

func TestResponseModifier(t *testing.T) {
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New(ResponseModifier(func(resp *http.Response) error {
		resp.Header.Add("X-Test", "CUSTOM")
		return nil
	}))
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	re, _, err := testutils.Get(proxy.URL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, re.StatusCode)
	assert.Equal(t, "CUSTOM", re.Header.Get("X-Test"))
}

func TestXForwardedHostHeader(t *testing.T) {
	tests := []struct {
		Description            string
		PassHostHeader         bool
		TargetURL              string
		ProxyfiedURL           string
		ExpectedXForwardedHost string
	}{
		{
			Description:            "XForwardedHost without PassHostHeader",
			PassHostHeader:         false,
			TargetURL:              "http://xforwardedhost.com",
			ProxyfiedURL:           "http://backend.com",
			ExpectedXForwardedHost: "xforwardedhost.com",
		},
		{
			Description:            "XForwardedHost with PassHostHeader",
			PassHostHeader:         true,
			TargetURL:              "http://xforwardedhost.com",
			ProxyfiedURL:           "http://backend.com",
			ExpectedXForwardedHost: "xforwardedhost.com",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.Description, func(t *testing.T) {
			t.Parallel()

			f, err := New(PassHostHeader(test.PassHostHeader))
			require.NoError(t, err)

			r, err := http.NewRequest(http.MethodGet, test.TargetURL, nil)
			require.NoError(t, err)
			backendURL, err := url.Parse(test.ProxyfiedURL)
			require.NoError(t, err)
			f.modifyRequest(r, backendURL)
			require.Equal(t, test.ExpectedXForwardedHost, r.Header.Get(XForwardedHost))
		})
	}
}

// Makes sure hop-by-hop headers are removed.
func TestForwardedHeaders(t *testing.T) {
	var outHeaders http.Header
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		outHeaders = req.Header
		_, _ = w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New(Rewriter(&HeaderRewriter{TrustForwardHeader: true, Hostname: "hello"}))
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	headers := http.Header{
		XForwardedProto:  []string{"httpx"},
		XForwardedFor:    []string{"192.168.1.1"},
		XForwardedServer: []string{"foobar"},
		XForwardedHost:   []string{"upstream-foobar"},
	}

	re, _, err := testutils.Get(proxy.URL, testutils.Headers(headers))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, re.StatusCode)
	assert.Equal(t, "httpx", outHeaders.Get(XForwardedProto))
	assert.Contains(t, outHeaders.Get(XForwardedFor), "192.168.1.1")
	assert.Contains(t, "upstream-foobar", outHeaders.Get(XForwardedHost))
	assert.Equal(t, "hello", outHeaders.Get(XForwardedServer))
}

func TestCustomRewriter(t *testing.T) {
	var outHeaders http.Header
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		outHeaders = req.Header
		_, _ = w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New(Rewriter(&HeaderRewriter{TrustForwardHeader: false, Hostname: "hello"}))
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	headers := http.Header{
		XForwardedProto: []string{"httpx"},
		XForwardedFor:   []string{"192.168.1.1"},
	}

	re, _, err := testutils.Get(proxy.URL, testutils.Headers(headers))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, re.StatusCode)
	assert.Equal(t, "http", outHeaders.Get(XForwardedProto))
	assert.NotContains(t, outHeaders.Get(XForwardedFor), "192.168.1.1")
}

func TestCustomTransportTimeout(t *testing.T) {
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		clock.Sleep(20 * clock.Millisecond)
		_, _ = w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New(RoundTripper(
		&http.Transport{
			ResponseHeaderTimeout: 5 * clock.Millisecond,
		}))
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	re, _, err := testutils.Get(proxy.URL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusGatewayTimeout, re.StatusCode)
}

func TestCustomLogger(t *testing.T) {
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New()
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	re, _, err := testutils.Get(proxy.URL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, re.StatusCode)
}

func TestRouteForwarding(t *testing.T) {
	var outPath string
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		outPath = req.RequestURI
		_, _ = w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New()
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	tests := []struct {
		Path  string
		Query string

		ExpectedPath string
	}{
		{"/hello", "", "/hello"},
		{"//hello", "", "//hello"},
		{"///hello", "", "///hello"},
		{"/hello", "abc=def&def=123", "/hello?abc=def&def=123"},
		{"/log/http%3A%2F%2Fwww.site.com%2Fsomething?a=b", "", "/log/http%3A%2F%2Fwww.site.com%2Fsomething?a=b"},
	}

	for _, test := range tests {
		proxyURL := proxy.URL + test.Path
		if test.Query != "" {
			proxyURL = proxyURL + "?" + test.Query
		}
		request, err := http.NewRequest("GET", proxyURL, nil)
		require.NoError(t, err)

		re, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, re.StatusCode)
		assert.Equal(t, test.ExpectedPath, outPath)
	}
}

func TestForwardedProto(t *testing.T) {
	var proto string
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		proto = req.Header.Get(XForwardedProto)
		_, _ = w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New()
	require.NoError(t, err)

	proxy := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	tproxy := httptest.NewUnstartedServer(proxy)
	tproxy.StartTLS()
	defer tproxy.Close()

	re, _, err := testutils.Get(tproxy.URL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, re.StatusCode)
	assert.Equal(t, "https", proto)
}

func TestContextWithValueInErrHandler(t *testing.T) {
	originalBool := false
	originalPBool := &originalBool

	type MyKey string
	const key MyKey = "test"

	f, err := New(ErrorHandler(utils.ErrorHandlerFunc(func(rw http.ResponseWriter, req *http.Request, err error) {
		test, isBool := req.Context().Value(key).(*bool)
		if isBool {
			*test = true
		}
		if err != nil {
			rw.WriteHeader(http.StatusBadGateway)
		}
	})))
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		// We need a network error
		req.URL = testutils.ParseURI("http://localhost:63450")
		newReq := req.WithContext(context.WithValue(req.Context(), key, originalPBool))

		f.ServeHTTP(w, newReq)
	})
	defer proxy.Close()

	re, _, err := testutils.Get(proxy.URL)
	require.NoError(t, err)

	assert.Equal(t, http.StatusBadGateway, re.StatusCode)
	assert.True(t, *originalPBool)
}

func TestTeTrailer(t *testing.T) {
	var teHeader string
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		teHeader = req.Header.Get(Te)
		_, _ = w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New()
	require.NoError(t, err)

	proxy := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	tproxy := httptest.NewUnstartedServer(proxy)
	tproxy.StartTLS()
	defer tproxy.Close()

	re, _, err := testutils.Get(tproxy.URL, testutils.Header("Te", "trailers"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, re.StatusCode)
	assert.Equal(t, "trailers", teHeader)
}

func TestUnannouncedTrailer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(200)
		rw.(http.Flusher).Flush()

		rw.Header().Add(http.TrailerPrefix+"X-Trailer", "foo")
	}))

	proxy, err := New()
	require.NoError(t, err)

	proxySrv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		proxy.ServeHTTP(rw, req)
	}))

	resp, _ := http.Get(proxySrv.URL)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, resp.Trailer.Get("X-Trailer"), "foo")
}

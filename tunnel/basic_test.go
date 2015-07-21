// Copyright (c) 2015 RightScale, Inc. - see LICENSE

package tunnel

// Omega: Alt+937

import (
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/ghttp"
	"gopkg.in/inconshreveable/log15.v2"
)

// Our simple proxy server. This server: only handles proxying of HTTPS data via
// CONNECT protocol, not HTTP. Also we don't bother to modify headers, such as
// adding X-Forwarded-For as we don't test that.
var proxyErrorLog string = ""
var proxyConnCount int = 0
var proxyServer *httptest.Server

func copyAndClose(w, r net.Conn) {
	connOk := true
	if _, err := io.Copy(w, r); err != nil {
		connOk = false
	}
	if err := r.Close(); err != nil && connOk {
		proxyErrorLog += fmt.Sprintf("Error closing: %s\n", err)
	}
}

func externalProxyServer(w http.ResponseWriter, r *http.Request) {
	//log.Printf("Proxy server got %#v\n", r)
	proxyConnCount += 1
	fmt.Printf("externalProxyServer proxying to %s %d\n", r.RequestURI, proxyConnCount)

	if r.Method != "CONNECT" {
		errMsg := "CONNECT not passed to proxy"
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(errMsg))
		proxyErrorLog += errMsg
		return
	}
	hij, ok := w.(http.Hijacker)
	if !ok {
		errMsg := "Typecast to hijack failed!"
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errMsg))
		proxyErrorLog += errMsg
		return
	}

	host := r.URL.Host
	targetSite, err := net.Dial("tcp", host)
	if err != nil {
		errMsg := "Cannot establish connection to upstream server!"
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errMsg))
		proxyErrorLog += errMsg
		return
	}

	proxyClient, _, err := hij.Hijack()
	if err != nil {
		errMsg := "Cannot Hijack connection!"
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errMsg))
		proxyErrorLog += errMsg
		return
	}

	res := fmt.Sprintf("%s 200 OK\r\n\r\n", r.Proto)
	proxyClient.Write([]byte(res))

	// Transparent pass through from now on
	go copyAndClose(targetSite, proxyClient)
	go copyAndClose(proxyClient, targetSite)
}

var startClient = func(wstunToken string, wstunHost string, proxy *url.URL, server *ghttp.Server) *WSTunnelClient {
	wstuncli := &WSTunnelClient{
		Token:          wstunToken,
		Tunnel:         "ws://" + wstunHost,
		Timeout:        30 * time.Second,
		Proxy:          proxy,
		Log:            log15.Root().New("pkg", "WStuncli"),
		InternalServer: server,
	}
	wstuncli.Start()
	fmt.Fprintf(os.Stderr, "Client started\n")
	return wstuncli
}

var _ = Describe("Testing requests", func() {

	var server *ghttp.Server
	var wstunsrv *WSTunnelServer
	var wstuncli *WSTunnelClient
	var wstunUrl string
	var wstunToken string
	var wstunHost string
	var proxyUrl *url.URL

	BeforeEach(func() {
		wstunToken = "test567890123456-" + strconv.Itoa(rand.Int()%1000000)
	})

	var runTests = func() {
		// Perform the test by running main() with the command line args set
		It("Responds to hello requests", func() {
			wstuncli = startClient(wstunToken, wstunHost, proxyUrl, server)

			server.AppendHandlers(
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/hello"),
					ghttp.RespondWith(200, `WORLD`, http.Header{"Content-Type": []string{"text/world"}}),
				),
			)

			resp, err := http.Get(wstunUrl + "/_token/" + wstunToken + "/hello")
			Ω(err).ShouldNot(HaveOccurred())
			respBody, err := ioutil.ReadAll(resp.Body)
			Ω(err).ShouldNot(HaveOccurred())
			Ω(string(respBody)).Should(Equal("WORLD"))
			Ω(resp.Header.Get("Content-Type")).Should(Equal("text/world"))
			Ω(resp.StatusCode).Should(Equal(200))
		})

		It("Gets error status", func() {
			wstuncli = startClient(wstunToken, wstunHost, proxyUrl, server)

			server.AppendHandlers(
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/hello"),
					ghttp.RespondWith(445, `WORLD`, http.Header{"Content-Type": []string{"text/world"}}),
				),
			)

			resp, err := http.Get(wstunUrl + "/_token/" + wstunToken + "/hello")
			Ω(err).ShouldNot(HaveOccurred())
			respBody, err := ioutil.ReadAll(resp.Body)
			Ω(err).ShouldNot(HaveOccurred())
			Ω(string(respBody)).Should(Equal("WORLD"))
			Ω(resp.Header.Get("Content-Type")).Should(Equal("text/world"))
			Ω(resp.StatusCode).Should(Equal(445))
		})

		It("Does 100 requests", func() {
			wstuncli = startClient(wstunToken, wstunHost, proxyUrl, server)

			const N = 100
			for i := 0; i < N; i++ {
				txt := fmt.Sprintf("/hello/%d", i)
				server.AppendHandlers(
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", txt),
						ghttp.RespondWith(200, txt,
							http.Header{"Content-Type": []string{"text/world"}}),
					),
				)
			}

			for i := 0; i < N; i++ {
				txt := fmt.Sprintf("/hello/%d", i)
				resp, err := http.Get(wstunUrl + "/_token/" + wstunToken + txt)
				Ω(err).ShouldNot(HaveOccurred())
				respBody, err := ioutil.ReadAll(resp.Body)
				Ω(err).ShouldNot(HaveOccurred())
				Ω(string(respBody)).Should(Equal(txt))
				Ω(resp.Header.Get("Content-Type")).Should(Equal("text/world"))
				Ω(resp.StatusCode).Should(Equal(200))
			}
		})

		It("Does many requests with random sleeps", func() {
			wstuncli = startClient(wstunToken, wstunHost, proxyUrl, server)

			const N = 20
			server.RouteToHandler("GET", regexp.MustCompile(`^/hello/`),
				func(w http.ResponseWriter, req *http.Request) {
					var i int
					n, err := fmt.Sscanf(req.RequestURI, "/hello/%d", &i)
					if n != 1 || err != nil {
						w.WriteHeader(400)
					} else {
						time.Sleep(time.Duration(10*i) * time.Millisecond)
						w.Header().Set("Content-Type", "text/world")
						w.WriteHeader(200)
						w.Write([]byte(fmt.Sprintf("/hello/%d", i)))
					}
				})

			resp := make([]*http.Response, N, N)
			err := make([]error, N, N)
			wg := sync.WaitGroup{}
			wg.Add(N)
			fmt.Fprintln(os.Stderr, "Launching N concurrent requests")
			for i := 0; i < N; i++ {
				go func(i int) {
					txt := fmt.Sprintf("/hello/%d", i)
					resp[i], err[i] = http.Get(wstunUrl + "/_token/" + wstunToken + txt)
					wg.Done()
				}(i)
			}
			wg.Wait()
			fmt.Fprintln(os.Stderr, "Evaluating the N requests")
			for i := 0; i < N; i++ {
				txt := fmt.Sprintf("/hello/%d", i)
				Ω(err[i]).ShouldNot(HaveOccurred())
				respBody, err := ioutil.ReadAll(resp[i].Body)
				Ω(err).ShouldNot(HaveOccurred())
				Ω(string(respBody)).Should(Equal(txt))
				Ω(resp[i].Header.Get("Content-Type")).Should(Equal("text/world"))
				Ω(resp[i].StatusCode).Should(Equal(200))
			}
		})
	}

	// wstunnel used as a go library integrated into another application
	Context("Internal requests", func() {
		var saved string
		BeforeEach(func() {
			saved = VV
			VV = "fooey"
			server = ghttp.NewUnstartedServer()

			l, _ := net.Listen("tcp", "127.0.0.1:0")
			wstunHost = l.Addr().String()
			wstunsrv = NewWSTunnelServer([]string{})
			wstunsrv.Start(l)
			wstunUrl = "http://" + wstunHost

			fmt.Fprintf(os.Stderr, "Server started\n")
		})
		AfterEach(func() {
			wstuncli.Stop()
			wstunsrv.Stop()
			server.Close()
			VV = saved
		})
		runTests()

		Context("with a proxy", func() {
			BeforeEach(func() {
				proxyServer = httptest.NewServer(http.HandlerFunc(externalProxyServer))
				proxyUrl, _ = url.Parse(proxyServer.URL)
				proxyErrorLog = ""
				proxyConnCount = 0
			})
			AfterEach(func() {
				proxyUrl = nil
				proxyServer.Close()
				Ω(proxyErrorLog).Should(Equal(""))
				Ω(proxyConnCount).Should(Equal(1))
			})
			runTests()
		})
	})

	// wstunnel connected to an external http service
	Context("Basic requests", func() {
		BeforeEach(func() {
			server = ghttp.NewServer()
			fmt.Fprintf(os.Stderr, "ghttp started on %s\n", server.URL())

			l, _ := net.Listen("tcp", "127.0.0.1:0")
			wstunHost = l.Addr().String()
			wstunsrv = NewWSTunnelServer([]string{})
			wstunsrv.Start(l)
			wstunUrl = "http://" + wstunHost

			fmt.Fprintf(os.Stderr, "Server started\n")

			startClient = func(wstunToken string, wstunHost string, proxy *url.URL, server *ghttp.Server) *WSTunnelClient {
				wstuncli = NewWSTunnelClient([]string{
					"-token", wstunToken,
					"-tunnel", "ws://" + wstunHost,
					"-server", server.URL(),
				})
				wstuncli.Start()
				fmt.Fprintf(os.Stderr, "Client started\n")
				return wstuncli
			}

		})
		AfterEach(func() {
			wstuncli.Stop()
			wstunsrv.Stop()
			server.Close()
		})
		runTests()
	})
})

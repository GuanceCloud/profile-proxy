package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Cfg struct {
	Endpoints    []string      `yaml:"endpoints"`
	BindAddr     string        `yaml:"bind_addr"`
	ProxyTimeout time.Duration `yaml:"proxy_timeout"`
	ProxyURIs    []string      `yaml:"proxy_uris"`
	proxyURIDict map[string]struct{}
}

func (c *Cfg) init() {
	if c.proxyURIDict == nil {
		c.proxyURIDict = make(map[string]struct{}, len(c.ProxyURIs))
	}

	for _, URI := range c.ProxyURIs {
		if !strings.HasPrefix(URI, "/") {
			URI += "/"
		}
		c.proxyURIDict[URI] = struct{}{}
	}
}

var C = &Cfg{
	Endpoints:    []string{"http://127.0.0.1:8126", "http://127.0.0.1:9529"},
	BindAddr:     ":2626",
	ProxyTimeout: time.Second * 45,
	ProxyURIs: []string{
		"/v0.4/traces",
		"/v0.5/traces",
		"/profiling/v1/input",
	},
}

var endpoint = ""

var client = &http.Client{
	Timeout: C.ProxyTimeout + time.Second*10,
}

type proxyEndpoint struct {
	raw      string
	resolved *url.URL
}

func ResolveEndpoints(endpoints []string) ([]*proxyEndpoint, error) {
	resolvedEndpoints := make([]*proxyEndpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		rawEp := ep
		if !strings.Contains(ep, "://") {
			ep = "http://" + ep
		}

		URL, err := url.Parse(ep)
		if err != nil {
			return nil, fmt.Errorf("illegal endpoint [%s]: %v", ep, err)
		}
		if URL.Scheme == "" {
			URL.Scheme = "http"
		}
		if URL.Path == "" {
			URL.Path = "/"
		}

		// ignore query and fragment
		URL.RawQuery, URL.Fragment, URL.RawFragment = "", "", ""
		resolvedEndpoints = append(resolvedEndpoints, &proxyEndpoint{
			raw:      rawEp,
			resolved: URL,
		})
	}
	return resolvedEndpoints, nil
}

func main() {

	flag.StringVar(&endpoint, "endpoint", "", "target endpoints, multiple split by comma")
	flag.Parse()

	C.init()
	if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
		endpoints := strings.Split(endpoint, ",")
		C.Endpoints = endpoints
	}

	http.DefaultClient.Timeout = time.Second * 30

	proxyEndpoints := make([]*proxyEndpoint, 0, len(C.Endpoints))
	for _, ep := range C.Endpoints {
		URL, err := url.ParseRequestURI(ep)
		if err != nil {
			panic(fmt.Sprintf("illegal endpoint [%s]: %v", ep, err))
		}
		if URL.Scheme == "" {
			URL.Scheme = "http"
		}
		if URL.Path == "" {
			URL.Path = "/"
		}

		// ignore query and fragment
		URL.RawQuery, URL.Fragment, URL.RawFragment = "", "", ""
		proxyEndpoints = append(proxyEndpoints, &proxyEndpoint{
			raw:      ep,
			resolved: URL,
		})
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := C.proxyURIDict[r.URL.Path]; !ok {
			w.WriteHeader(http.StatusOK)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			log.Printf("unable to read incoming request [%s] body: %v \n", r.URL.String(), err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		var (
			out        []byte
			outHeaders http.Header
			mutex      sync.Mutex
		)
		wg := new(sync.WaitGroup)
		wg.Add(len(proxyEndpoints))

		for _, epURL := range proxyEndpoints {
			go func(epURL *proxyEndpoint) {
				defer wg.Done()
				resp, cancelFN, ReadErr := proxy(r, body, epURL)
				if cancelFN != nil {
					defer cancelFN()
				}
				if ReadErr != nil {
					log.Printf("unable to proxy request [%s] to endpoint [%s]: %v \n", r.URL.String(), epURL.raw, ReadErr)
					return
				}
				defer resp.Body.Close()

				respBody, ReadErr := io.ReadAll(resp.Body)
				if ReadErr != nil {
					log.Printf("unable to read response body: %v when proxy request [%s] to endpoint [%s] \n", ReadErr, r.URL.String(), epURL.raw)
				}

				if resp.StatusCode/100 != 2 {
					log.Printf("unable to proxy request [%s] to endpoint [%s]: server return error status: %s, response body: %s \n",
						r.URL.String(), epURL.raw, resp.Status, respBody)
					return
				}

				log.Printf("Successfully proxy request [%s] to endpoint [%s], response body: %s \n", r.URL.String(), epURL.raw, respBody)

				if ReadErr == nil {
					mutex.Lock()
					defer mutex.Unlock()
					if len(out) < len(respBody) {
						out = respBody
						outHeaders = resp.Header.Clone()
					}
				}
			}(epURL)
		}

		wg.Wait()
		if out != nil {
			for k, vv := range outHeaders {
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			w.Write(out)
		}
	})

	fmt.Printf("the proxy is listening at %s\n", C.BindAddr)
	if err := http.ListenAndServe(C.BindAddr, nil); err != nil {
		log.Fatal(err)
	}
}

func joinURL(a, b string) string {
	a = strings.TrimRight(a, "/")
	b = strings.TrimLeft(b, "/")
	return a + "/" + b
}

func proxy(r *http.Request, body []byte, ep *proxyEndpoint) (*http.Response, context.CancelFunc, error) {
	proxyURL, err := url.Parse(joinURL(ep.resolved.String(), r.URL.RequestURI()))
	if err != nil {
		return nil, nil, fmt.Errorf("unable to build proxy target URL: %w", err)
	}

	ctx, fn := context.WithTimeout(context.Background(), C.ProxyTimeout)
	newReq := r.Clone(ctx)
	newReq.URL = proxyURL
	newReq.Host = proxyURL.Host

	// For client requests, a value of 0 with a non-nil Body is
	// also treated as unknown.
	newReq.ContentLength = 0

	// It is an error to set this field in an HTTP client request.
	newReq.RequestURI = ""

	newReq.Body = io.NopCloser(bytes.NewReader(body))
	newReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	resp, err := client.Do(newReq)
	return resp, fn, err
}

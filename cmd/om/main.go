package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/holon-sh/om/compile"
)

func main() {
	if len(os.Args) != 3 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "Usage: om serve <OpenAPI path or HTTPS URL>")
		os.Exit(2)
	}
	if err := serve(context.Background(), os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "om:", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, source string) error {
	data, location, err := load(ctx, source)
	if err != nil {
		return err
	}
	document, omitted, err := compile.Compile(ctx, data, location)
	if err != nil {
		return err
	}
	for _, omission := range omitted {
		fmt.Fprintln(os.Stderr, "Skipped", omission)
	}
	for _, input := range document.Inputs {
		if _, set := os.LookupEnv(input.Name); input.Secret && !set {
			fmt.Fprintln(os.Stderr, "Set", input.Name, "to supply credentials for tools that need it.")
		}
	}
	handler, err := document.Handler(os.LookupEnv, http.DefaultTransport)
	if err != nil {
		return err
	}
	host := cmp.Or(os.Getenv("HOST"), "127.0.0.1")
	listener, err := net.Listen("tcp", net.JoinHostPort(host, cmp.Or(os.Getenv("PORT"), "3000")))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	var root http.Handler = mux
	if local(host) {
		root = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin, err := url.Parse(r.Header.Get("Origin"))
			if !local((&url.URL{Host: r.Host}).Hostname()) || err != nil || r.Header.Get("Origin") != "" && !local(origin.Hostname()) {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			mux.ServeHTTP(w, r)
		})
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	fmt.Printf("MCP server listening at http://%s/mcp\n", net.JoinHostPort(host, port))
	return http.Serve(listener, root)
}

func local(hostname string) bool {
	return slices.Contains([]string{"127.0.0.1", "::1", "localhost"}, hostname)
}

const documentLimit = 32 << 20

func load(ctx context.Context, source string) ([]byte, *url.URL, error) {
	if !strings.Contains(source, "://") {
		file, err := os.Open(source)
		if err != nil {
			return nil, nil, err
		}
		defer file.Close()
		data, err := readDocument(file)
		return data, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil || !httpsURL(req.URL) {
		return nil, nil, errors.New("document URL must be HTTPS without credentials or a fragment")
	}
	client := http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || !httpsURL(req.URL) {
				return errors.New("invalid document redirect")
			}
			return nil
		},
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("document request returned HTTP %d", response.StatusCode)
	}
	data, err := readDocument(response.Body)
	return data, response.Request.URL, err
}

func httpsURL(u *url.URL) bool {
	return u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == ""
}

func readDocument(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, documentLimit+1))
	if len(data) > documentLimit {
		return nil, errors.New("OpenAPI document exceeds 32 MiB")
	}
	return data, err
}

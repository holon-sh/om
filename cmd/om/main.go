package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/holon-sh/om"
)

func main() {
	if len(os.Args) != 4 || os.Args[1] != "generate" {
		fmt.Fprintln(os.Stderr, "Usage: om generate <OpenAPI path or HTTPS URL> <new directory>")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := generate(ctx, os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, "om:", err)
		os.Exit(1)
	}
}

func generate(ctx context.Context, source, out string) error {
	target, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	data, location, err := document(ctx, source)
	if err != nil {
		return err
	}
	project, err := om.Generate(ctx, data, location)
	if err != nil {
		return err
	}
	if err := writeProject(target, project.Files); err != nil {
		return err
	}
	for _, omitted := range project.Omitted {
		fmt.Fprintln(os.Stderr, "Skipped", omitted)
	}
	for _, input := range project.Inputs {
		if input.Secret {
			fmt.Fprintln(os.Stderr, "Set", input.Name, "to supply credentials for tools that need it.")
		}
	}
	fmt.Printf("Generated %s\nRun npm ci and npm start in the project directory. MCP listens at http://127.0.0.1:3000/mcp.\n", out)
	return nil
}

const documentLimit = 32 << 20

func document(ctx context.Context, source string) ([]byte, *url.URL, error) {
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

func writeProject(target string, files map[string][]byte) (err error) {
	if err := os.Mkdir(target, 0755); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(target))
		}
	}()
	for path, data := range files {
		if err := os.WriteFile(filepath.Join(target, path), data, 0644); err != nil {
			return err
		}
	}
	return nil
}

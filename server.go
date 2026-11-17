package om

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

type variable struct {
	Name    string   `json:"name"`
	Env     string   `json:"env"`
	Default string   `json:"default"`
	Allowed []string `json:"allowed,omitempty"`
}

type resolvedServer struct {
	url       string
	variables []variable
	inputs    map[string]input
	endpoint  Endpoint
}

var placeholder = regexp.MustCompile(`\{([^{}]+)\}`)
var dnsLabel = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func resolveServer(servers openapi3.Servers, location *url.URL) (resolvedServer, error) {
	server := &openapi3.Server{URL: "/"}
	if len(servers) != 0 {
		server = servers[0]
	}
	result := resolvedServer{variables: []variable{}, inputs: map[string]input{}}
	markers := map[string]string{}
	var tokens, original []string
	for _, match := range placeholder.FindAllStringSubmatch(server.URL, -1) {
		name := match[1]
		if _, exists := markers[name]; exists {
			continue
		}
		declared := server.Variables[name]
		if declared == nil {
			return resolvedServer{}, fmt.Errorf("server URL variable %q is undeclared", name)
		}
		if strings.ContainsAny(declared.Default, "\r\n\x00") {
			return resolvedServer{}, fmt.Errorf("server URL variable %q has an invalid default", name)
		}
		marker := fmt.Sprintf("om-variable-%d-slot", len(markers))
		for strings.Contains(server.URL, marker) || location != nil && strings.Contains(location.String(), marker) {
			marker += "x"
		}
		markers[name] = marker
		tokens = append(tokens, match[0], marker)
		original = append(original, marker, match[0])
		variable := variable{Name: name, Env: envName(name), Default: declared.Default, Allowed: declared.Enum}
		result.variables = append(result.variables, variable)
		if err := addInput(result.inputs, input{
			Input:  Input{Name: variable.Env, Description: declared.Description, Default: &declared.Default, Choices: declared.Enum},
			source: "server variable " + name,
		}); err != nil {
			return resolvedServer{}, err
		}
	}
	address, err := url.Parse(strings.NewReplacer(tokens...).Replace(server.URL))
	if err != nil {
		return resolvedServer{}, errors.New("invalid OpenAPI server URL")
	}
	if location != nil {
		address = location.ResolveReference(address)
	}
	if address.Scheme != "https" || address.Hostname() == "" || address.User != nil ||
		address.RawQuery != "" || address.ForceQuery || address.Fragment != "" {
		return resolvedServer{}, errors.New("OpenAPI server must resolve to an HTTPS URL without credentials, query or fragment")
	}
	port := 443
	if rawPort := address.Port(); rawPort != "" {
		port, err = strconv.Atoi(rawPort)
		if err != nil || port < 1 || port > 65535 {
			return resolvedServer{}, errors.New("OpenAPI server port must be between 1 and 65535")
		}
	}
	host := strings.ToLower(address.Hostname())
	for _, v := range result.variables {
		marker := markers[v.Name]
		if !strings.Contains(host, marker) {
			continue
		}
		if !validHostname(v.Default) {
			return resolvedServer{}, fmt.Errorf("server hostname variable %q has an invalid default", v.Name)
		}
		for _, value := range v.Allowed {
			if !validHostname(value) {
				return resolvedServer{}, fmt.Errorf("server hostname variable %q has an invalid choice", v.Name)
			}
		}
	}
	if !validHostname(host) {
		return resolvedServer{}, errors.New("OpenAPI server has an invalid hostname")
	}
	for _, v := range result.variables {
		host = strings.ReplaceAll(host, markers[v.Name], "{"+v.Env+"}")
	}
	result.url = strings.NewReplacer(original...).Replace(address.String())
	result.endpoint = Endpoint{Host: host, Port: port}
	return result, nil
}

func validHostname(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !dnsLabel.MatchString(label) {
			return false
		}
	}
	return true
}

package om

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
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
	var withMarkers, withDefaults, withNames, withEnv []string
	for _, match := range placeholder.FindAllStringSubmatch(server.URL, -1) {
		name := match[1]
		if slices.ContainsFunc(result.variables, func(v variable) bool { return v.Name == name }) {
			continue
		}
		declared := server.Variables[name]
		if declared == nil {
			return resolvedServer{}, fmt.Errorf("server URL variable %q is undeclared", name)
		}
		v := variable{Name: name, Env: envName(name), Default: declared.Default, Allowed: declared.Enum}
		err := addInputs(result.inputs, input{
			Input:  Input{Name: v.Env, Description: declared.Description, Default: &declared.Default, Choices: declared.Enum},
			source: "server variable " + name,
		})
		if err != nil {
			return resolvedServer{}, err
		}
		marker := fmt.Sprintf("om-variable-%d", len(result.variables))
		result.variables = append(result.variables, v)
		withMarkers = append(withMarkers, match[0], marker)
		withDefaults = append(withDefaults, marker, v.Default)
		withNames = append(withNames, marker, match[0])
		withEnv = append(withEnv, marker, "{"+v.Env+"}")
	}
	address, err := url.Parse(strings.NewReplacer(withMarkers...).Replace(server.URL))
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
	if !validHostname(strings.NewReplacer(withDefaults...).Replace(host)) {
		return resolvedServer{}, errors.New("OpenAPI server has an invalid hostname")
	}
	result.url = strings.NewReplacer(withNames...).Replace(address.String())
	result.endpoint = Endpoint{Host: strings.NewReplacer(withEnv...).Replace(host), Port: port}
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

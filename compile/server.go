package compile

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
	"github.com/holon-sh/om"
)

var placeholder = regexp.MustCompile(`\{([^{}]+)\}`)
var dnsLabel = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func resolveServer(servers openapi3.Servers, location *url.URL) (string, map[string]input, error) {
	server := &openapi3.Server{URL: "/"}
	if len(servers) != 0 {
		server = servers[0]
	}
	inputs := map[string]input{}
	var names []string
	var withMarkers, withDefaults, withInputs []string
	for _, match := range placeholder.FindAllStringSubmatch(server.URL, -1) {
		name := match[1]
		if slices.Contains(names, name) {
			continue
		}
		declared := server.Variables[name]
		if declared == nil {
			return "", nil, fmt.Errorf("server URL variable %q is undeclared", name)
		}
		variable := input{
			Input:  om.Input{Name: envName(name), Default: &declared.Default, Choices: declared.Enum},
			source: "server variable " + name,
		}
		if err := addInputs(inputs, variable); err != nil {
			return "", nil, err
		}
		marker := fmt.Sprintf("om-variable-%d", len(names))
		names = append(names, name)
		withMarkers = append(withMarkers, match[0], marker)
		withDefaults = append(withDefaults, marker, declared.Default)
		withInputs = append(withInputs, marker, "{"+variable.Name+"}")
	}
	address, err := url.Parse(strings.NewReplacer(withMarkers...).Replace(server.URL))
	if err != nil {
		return "", nil, errors.New("invalid OpenAPI server URL")
	}
	if location != nil {
		address = location.ResolveReference(address)
	}
	if address.Scheme != "https" || address.Hostname() == "" || address.User != nil ||
		address.RawQuery != "" || address.ForceQuery || address.Fragment != "" {
		return "", nil, errors.New("OpenAPI server must resolve to an HTTPS URL without credentials, query or fragment")
	}
	if port := address.Port(); port != "" {
		if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
			return "", nil, errors.New("OpenAPI server port must be between 1 and 65535")
		}
	}
	address.Host = strings.ToLower(address.Host)
	if !validHostname(strings.NewReplacer(withDefaults...).Replace(address.Hostname())) {
		return "", nil, errors.New("OpenAPI server has an invalid hostname")
	}
	return strings.NewReplacer(withInputs...).Replace(address.String()), inputs, nil
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

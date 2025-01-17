package dynatrace_client

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
)

var log = logf.Log.WithName("oneagent.client")

// Client is the interface for the Dynatrace REST API client.
type Client interface {
	// GetVersionForLatest gets the latest agent version for the given OS and installer type.
	// Returns the version as received from the server on success.
	//
	// Returns an error for the following conditions:
	//  - os or installerType is empty
	//  - IO error or unexpected response
	//  - error response from the server (e.g. authentication failure)
	//  - the agent version is not set or empty
	GetVersionForLatest(os, installerType string) (string, error)

	// GetVersionForIp returns the agent version running on the host with the given IP address.
	// Returns the version string formatted as "Major.Minor.Revision.Timestamp" on success.
	//
	// Returns an error for the following conditions:
	//  - the IP is empty
	//  - IO error or unexpected response
	//  - error response from the server (e.g. authentication failure)
	//  - a host with the given IP cannot be found
	//  - the agent version for the host is not set
	//
	// The list of all hosts with their IP addresses is cached the first time this method is called. Use a new
	// client instance to fetch a new list from the server.
	GetVersionForIp(ip string) (string, error)

	// GetCommunicationHosts returns, on success, the list of communication hosts used for available
	// communication endpoints that the Dynatrace OneAgent can use to connect to.
	//
	// Returns an error if there was also an error response from the server.
	GetCommunicationHosts() ([]CommunicationHost, error)

	// GetAPIURLHost returns a CommunicationHost for the client's API URL. Or error, if failed to be parsed.
	GetAPIURLHost() (CommunicationHost, error)
}

// CommunicationHost represents a host used in a communication endpoint.
type CommunicationHost struct {
	Protocol string
	Host     string
	Port     uint32
}

// Known OS values.
const (
	OsWindows = "windows"
	OsUnix    = "unix"
	OsAix     = "aix"
	OsSolaris = "solaris"
)

// Known installer types.
const (
	InstallerTypeDefault    = "default"
	InstallerTypeUnattended = "default-unattended"
	InstallerTypePaasZip    = "paas"
	InstallerTypePaasSh     = "paas-sh"
)

// NewClient creates a REST client for the given API base URL and authentication tokens.
// Returns an error if a token or the URL is empty.
//
// The API base URL is different for managed and SaaS environments:
//  - SaaS: https://{environment-id}.live.dynatrace.com/api
//  - Managed: https://{domain}/e/{environment-id}/api
//
// opts can be used to customize the created client, entries must not be nil.
func NewClient(url, apiToken, paasToken string, opts ...Option) (Client, error) {
	if len(url) == 0 {
		return nil, errors.New("url is empty")
	}
	if len(apiToken) == 0 || len(paasToken) == 0 {
		return nil, errors.New("token is empty")
	}

	if strings.HasSuffix(url, "/") {
		url = url[:len(url)-1]
	}

	c := &client{
		url:       url,
		apiToken:  apiToken,
		paasToken: paasToken,

		httpClient: http.DefaultClient,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Option can be passed to NewClient and customizes the created client instance.
type Option func(*client)

// SkipCertificateValidation creates an Option that specifies whether validation of the server's TLS
// certificate should be skipped. The default is false.
func SkipCertificateValidation(skip bool) Option {
	return func(c *client) {
		if skip {
			c.httpClient = &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				},
			}
		} else {
			c.httpClient = http.DefaultClient
		}
	}
}

// client implements the Client interface.
type client struct {
	url       string
	apiToken  string
	paasToken string

	httpClient *http.Client

	hostCache map[string]string
}

// GetVersionForLatest gets the latest agent version for the given OS and installer type.
func (c *client) GetVersionForLatest(os, installerType string) (string, error) {
	if len(os) == 0 || len(installerType) == 0 {
		return "", errors.New("os or installerType is empty")
	}

	resp, err := c.makeRequest("%s/v1/deployment/installer/agent/%s/%s/latest/metainfo?Api-Token=%s",
		c.url, os, installerType, c.paasToken)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	return readLatestVersion(resp.Body)
}

// GetVersionForIp returns the agent version running on the host with the given IP address.
func (c *client) GetVersionForIp(ip string) (string, error) {
	if len(ip) == 0 {
		return "", errors.New("ip is invalid")
	}

	if c.hostCache == nil {
		resp, err := c.makeRequest("%s/v1/entity/infrastructure/hosts?Api-Token=%s&includeDetails=false", c.url, c.apiToken)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()

		c.hostCache, err = readHostMap(resp.Body)
		if err != nil {
			return "", err
		}
	}

	switch v, ok := c.hostCache[ip]; {
	case !ok:
		return "", errors.New("host not found")
	case v == "":
		return "", errors.New("agent version not set for host")
	default:
		return v, nil
	}
}

func (c *client) GetAPIURLHost() (CommunicationHost, error) {
	return parseEndpoint(c.url)
}

// GetCommunicationHosts returns the hosts used in the communication endpoints available on the environment.
func (c *client) GetCommunicationHosts() ([]CommunicationHost, error) {
	resp, err := c.makeRequest("%s/v1/deployment/installer/agent/connectioninfo?Api-Token=%s", c.url, c.paasToken)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return readCommunicationHosts(resp.Body)
}

// makeRequest does an HTTP request by formatting the URL from the given arguments and returns the response.
// The response body must be closed by the caller when no longer used.
func (c *client) makeRequest(format string, a ...interface{}) (*http.Response, error) {
	url := fmt.Sprintf(format, a...)
	return c.httpClient.Get(url)
}

// serverError represents an error returned from the server (e.g. authentication failure).
type serverError struct {
	Code    float64
	Message string
}

// Error formats the server error code and message.
func (e serverError) Error() string {
	if len(e.Message) == 0 && e.Code == 0 {
		return "unknown server error"
	}
	return fmt.Sprintf("error %d: %s", int64(e.Code), e.Message)
}

// readLatestVersion reads the agent version from the given server response reader.
func readLatestVersion(r io.Reader) (string, error) {
	type jsonResponse struct {
		LatestAgentVersion string

		Error *serverError
	}

	var resp jsonResponse
	switch err := json.NewDecoder(r).Decode(&resp); {
	case err != nil:
		return "", err
	case resp.Error != nil:
		return "", resp.Error
	}

	v := resp.LatestAgentVersion
	if len(v) == 0 {
		return "", errors.New("agent version not set")
	}
	return v, nil
}

// readHostMap builds a map from IP address to host version by reading from the given server response reader.
func readHostMap(r io.Reader) (map[string]string, error) {
	type jsonHost struct {
		IpAddresses  []string
		AgentVersion *struct {
			Major     int
			Minor     int
			Revision  int
			Timestamp string
		}
	}

	buf := bufio.NewReader(r)
	// Server sends an array of hosts or an error object, check which one it is
	switch b, err := buf.Peek(1); {
	case err != nil:
		return nil, err

	case b[0] == '{':
		// Try decoding an error response
		var resp struct{ Error *serverError }
		switch err = json.NewDecoder(buf).Decode(&resp); {
		case err != nil:
			return nil, err
		case resp.Error != nil:
			return nil, resp.Error
		default:
			return nil, errors.New("unexpected response from server")
		}

	case b[0] != '[':
		return nil, errors.New("unexpected response from server")
	}

	dec := json.NewDecoder(buf)
	// Consume opening bracket
	if _, err := dec.Token(); err != nil {
		return nil, err
	}

	result := map[string]string{}
	for dec.More() {
		var host jsonHost
		if err := dec.Decode(&host); err != nil {
			return nil, err
		}

		var version string
		if v := host.AgentVersion; v != nil {
			version = fmt.Sprintf("%d.%d.%d.%s", v.Major, v.Minor, v.Revision, v.Timestamp)
		}
		for _, ip := range host.IpAddresses {
			result[ip] = version
		}
	}

	// Consume closing bracket
	if _, err := dec.Token(); err != nil {
		return nil, err
	}

	return result, nil
}

// readCommunicationHosts returns the list of communication hosts used on communication endpoints
// for the environment.
func readCommunicationHosts(r io.Reader) ([]CommunicationHost, error) {
	type jsonResponse struct {
		CommunicationEndpoints []string

		Error *serverError
	}

	var resp jsonResponse
	switch err := json.NewDecoder(r).Decode(&resp); {
	case err != nil:
		return nil, err
	case resp.Error != nil:
		return nil, resp.Error
	}

	out := make([]CommunicationHost, 0, len(resp.CommunicationEndpoints))

	for _, s := range resp.CommunicationEndpoints {
		logger := log.WithValues("url", s)

		e, err := parseEndpoint(s)
		if err != nil {
			logger.Info("failed to parse communication endpoint")
			continue
		}

		out = append(out, e)
	}

	if len(out) == 0 {
		return nil, errors.New("no hosts available")
	}

	return out, nil
}

func parseEndpoint(s string) (CommunicationHost, error) {
	u, err := url.ParseRequestURI(s)
	if err != nil {
		return CommunicationHost{}, errors.New("failed to parse URL")
	}

	if u.Scheme == "" {
		return CommunicationHost{}, errors.New("no protocol provided")
	} else if u.Scheme != "http" && u.Scheme != "https" {
		return CommunicationHost{}, errors.New("unknown protocol")
	}

	rp := u.Port() // Empty if not included in the URI

	var p uint32
	if rp == "" {
		switch u.Scheme {
		case "http":
			p = 80
		case "https":
			p = 443
		}
	} else {
		if q, err := strconv.ParseUint(rp, 10, 32); err != nil {
			return CommunicationHost{}, errors.New("failed to parse port")
		} else {
			p = uint32(q)
		}
	}

	return CommunicationHost{
		Protocol: u.Scheme,
		Host:     u.Hostname(),
		Port:     p,
	}, nil
}

// Copyright 2016 Mender Software AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"net/http"

	"github.com/mendersoftware/log"
)

var (
	errorLoadingClientCertificate      = errors.New("Failed to load certificate and key")
	errorNoServerCertificateFound      = errors.New("No server certificate is provided, use -trusted-certs with a proper certificate.")
	errorAddingServerCertificateToPool = errors.New("Error adding trusted server certificate to pool.")
)

const (
	minimumImageSize int64 = 4096 //kB
)

type RequestProcessingFunc func(response *http.Response) (interface{}, error)

type Updater interface {
	GetScheduledUpdate(RequestProcessingFunc, string) (interface{}, error)
	FetchUpdate(string) (io.ReadCloser, int64, error)
}

// Client represents the http(s) client used for network communication.
//
type httpClient struct {
	HTTPClient   *http.Client
	minImageSize int64
}

type httpsClient struct {
	httpClient
	httpsClientAuthCreds
}

// Client initialization

func NewUpdater(conf httpsClientConfig) Updater {
	if conf == (httpsClientConfig{}) {
		return NewHttpClient()
	}
	return NewHttpsClient(conf)
}

func NewHttpClient() *httpClient {
	var client httpClient
	client.minImageSize = minimumImageSize
	client.HTTPClient = &http.Client{}

	return &client
}

func NewHttpsClient(conf httpsClientConfig) *httpsClient {
	var client httpsClient
	client.httpClient = *NewHttpClient()

	if err := client.initServerTrust(conf); err != nil {
		return nil
	}

	if err := client.initClientCert(conf); err != nil {
		return nil
	}

	transport := http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:      &client.trustedCerts,
			Certificates: []tls.Certificate{client.clientCert},
		},
	}

	client.HTTPClient.Transport = &transport
	return &client
}

// Client configuration

type httpsClientConfig struct {
	certFile   string
	certKey    string
	serverCert string
}

type httpsClientAuthCreds struct {
	// Cert+privkey that authenticates this client
	clientCert tls.Certificate
	// Trusted server certificates
	trustedCerts x509.CertPool
}

func (c *httpsClient) initServerTrust(conf httpsClientConfig) error {
	if conf.serverCert == "" {
		// TODO: this is for pre-production version only to simplify tests.
		// Make sure to remove in production version.
		log.Warn("Server certificate not provided. Trusting all servers.")
		return nil
	}

	c.trustedCerts = *x509.NewCertPool()
	// Read certificate file.
	cacert, err := ioutil.ReadFile(conf.serverCert)
	if err != nil {
		return err
	}
	c.trustedCerts.AppendCertsFromPEM(cacert)

	if len(c.trustedCerts.Subjects()) == 0 {
		return errorAddingServerCertificateToPool
	}
	return nil
}

func (c *httpsClient) initClientCert(conf httpsClientConfig) error {
	if conf.certFile == "" || conf.certKey == "" {
		// TODO: this is for pre-production version only to simplify tests.
		// Make sure to remove in production version.
		log.Warn("No client key and certificate provided. Using auto-generated.")
		return nil
	}

	clientCert, err := tls.LoadX509KeyPair(conf.certFile, conf.certKey)
	if err != nil {
		return errorLoadingClientCertificate
	}
	c.clientCert = clientCert
	return nil
}

func (c *httpClient) GetScheduledUpdate(process RequestProcessingFunc, server string) (interface{}, error) {
	r, err := c.makeAndSendRequest(http.MethodGet, server)
	if err != nil {
		return nil, err
	}

	defer r.Body.Close()

	return process(r)
}

// Returns a byte stream which is a download of the given link.
func (c *httpClient) FetchUpdate(url string) (io.ReadCloser, int64, error) {
	r, err := c.makeAndSendRequest(http.MethodGet, url)
	if err != nil {
		return nil, -1, err
	}

	if r.ContentLength < 0 {
		return nil, -1, errors.New("Will not continue with unknown image size.")
	} else if r.ContentLength < c.minImageSize {
		return nil, -1, errors.New("Less than " + string(c.minImageSize) + "KiB image update (" +
			string(r.ContentLength) + " bytes)? Something is wrong, aborting.")
	}

	return r.Body, r.ContentLength, nil
}

func (client *httpClient) makeAndSendRequest(method, url string) (*http.Response, error) {

	res, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}

	log.Debug("Sending HTTP [", method, "] request: ", url)
	return client.HTTPClient.Do(res)
}

// possible API responses received for update request
const (
	updateResponseHaveUpdate = 200
	updateResponseNoUpdates  = 204
	updateResponseError      = 404
)

// have update for the client
type UpdateResponse struct {
	Image struct {
		URI      string
		Checksum string
		ID       string
	}
	ID string
}

func validateGetUpdate(update UpdateResponse) error {
	// check if we have JSON data correctky decoded
	if update.ID != "" && update.Image.ID != "" && update.Image.Checksum != "" && update.Image.URI != "" {
		log.Info("Received correct request for getting image from: " + update.Image.URI)
		return nil
	}
	return errors.New("Missing parameters in encoded JSON response")
}

func processUpdateResponse(response *http.Response) (interface{}, error) {
	log.Debug("Received response:", response.Status)

	respBody, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}

	switch response.StatusCode {
	case updateResponseHaveUpdate:
		log.Debug("Have update available")

		data := new(UpdateResponse)
		if err := json.Unmarshal(respBody, data); err != nil {
			switch err.(type) {
			case *json.SyntaxError:
				return nil, errors.New("Error parsing data syntax")
			}
			return nil, errors.New("Error parsing data: " + err.Error())
		}
		if err := validateGetUpdate(*data); err != nil {
			return nil, err
		}
		return data, nil

	case updateResponseNoUpdates:
		log.Debug("No update available")
		return nil, nil

	case updateResponseError:
		return nil, errors.New("Client not authorized to get update schedule.")

	default:
		return nil, errors.New("Invalid response received from server")
	}
}

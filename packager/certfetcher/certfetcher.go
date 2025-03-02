// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package certfetcher

import (
	"crypto"
	"crypto/x509"
	"strconv"

	"github.com/WICG/webpackage/go/signedexchange"
	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge/http01"
	"github.com/go-acme/lego/v4/challenge/tlsalpn01"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/http/webroot"
	"github.com/go-acme/lego/v4/registration"
	"github.com/pkg/errors"
)

type CertFetcher struct {
	AcmeDiscoveryURL string
	AcmeUser         AcmeUser
	legoClient       *lego.Client
	CertSignRequest  *x509.CertificateRequest
}

// Implements registration.User
type AcmeUser struct {
	Email        string
	Registration *registration.Resource
	key          crypto.PrivateKey
}

func (u *AcmeUser) GetEmail() string {
	return u.Email
}
func (u AcmeUser) GetRegistration() *registration.Resource {
	return u.Registration
}
func (u *AcmeUser) GetPrivateKey() crypto.PrivateKey {
	return u.key
}

// Initializes the cert fetcher with information it needs to fetch new certificates in the future.
// TODO(banaag): per gregable@ comments:
// Callsite could have some structure like:
//
// fetcher := CertFetcher()
// fetcher.setUser(email, privateKey)
// fetcher.bindToPort(port)
func New(email string, eabKid string, eabHmac string, certSignRequest *x509.CertificateRequest,
	privateKey crypto.PrivateKey, acmeDiscoURL string, httpChallengePort int, httpChallengeWebRoot string,
	tlsChallengePort int, dnsProvider string, shouldRegister bool) (*CertFetcher, error) {

	acmeUser := AcmeUser{
		Email: email,
		key:   privateKey,
	}
	config := lego.NewConfig(&acmeUser)

	config.CADirURL = acmeDiscoURL
	config.Certificate.KeyType = certcrypto.EC256

	client, err := NewLegoClient(config, httpChallengePort, httpChallengeWebRoot, tlsChallengePort, dnsProvider)
	if err != nil {
		return nil, errors.Wrap(err, "Setting up ACME challenges.")
	}

	var reg *registration.Resource
	if !shouldRegister {
		acmeUser.Registration = new(registration.Resource)
	} else if reg, err = client.Registration.ResolveAccountByKey(); err == nil {
		// Check if we already have an account.
		acmeUser.Registration = reg
	} else {
		// We need to reset the LEGO client after calling Registration.ResolveAccountByKey().
		client, err = NewLegoClient(config, httpChallengePort, httpChallengeWebRoot, tlsChallengePort, dnsProvider)
		if err != nil {
			return nil, errors.Wrap(err, "Setting up ACME challenges.")
		}

		// TODO(banaag) make sure we present the TOS URL to the user and prompt for confirmation.
		// The plan is to move this to some separate setup command outside the server which would be
		// executed one time. Alternatively, we can have a field in the toml file that is documented
		// to indicate agreement with TOS.
		if eabKid == "" && eabHmac == "" {
			reg, err = client.Registration.Register(registration.RegisterOptions{
				TermsOfServiceAgreed: true})
		} else {
			reg, err = client.Registration.RegisterWithExternalAccountBinding(registration.RegisterEABOptions{
				TermsOfServiceAgreed: true,
				Kid:                  eabKid,
				HmacEncoded:          eabHmac})
		}

		if err != nil {
			return nil, errors.Wrap(err, "ACME CA client registration")
		}
		acmeUser.Registration = reg
	}

	return &CertFetcher{
		AcmeDiscoveryURL: acmeDiscoURL,
		AcmeUser:         acmeUser,
		legoClient:       client,
		CertSignRequest:  certSignRequest,
	}, nil
}

// NewLegoClient returns a new Lego ACME Client given the configuration parameters passed in.
func NewLegoClient(config *lego.Config, httpChallengePort int,
	httpChallengeWebRoot string, tlsChallengePort int,
	dnsProvider string) (*lego.Client, error) {
	// A client facilitates communication with the CA server.
	client, err := lego.NewClient(config)
	if err != nil {
		return nil, errors.Wrap(err, "Obtaining LEGO client.")
	}

	// We specify an http port of `httpChallengePort`
	// because we aren't running as root and can't bind a listener to port 80 and 443
	// (used later when we attempt to pass challenges). Keep in mind that you still
	// need to proxy challenge traffic to port `acmeChallengePort`.
	if httpChallengePort != 0 {
		err := client.Challenge.SetHTTP01Provider(
			http01.NewProviderServer("", strconv.Itoa(httpChallengePort)))
		if err != nil {
			return nil, errors.Wrap(err, "Setting up HTTP01 challenge provider.")
		}
	}
	if httpChallengeWebRoot != "" {
		httpProvider, err := webroot.NewHTTPProvider(httpChallengeWebRoot)
		if err != nil {
			return nil, errors.Wrap(err, "Getting HTTP01 challenge provider.")
		}
		err = client.Challenge.SetHTTP01Provider(httpProvider)
		if err != nil {
			return nil, errors.Wrap(err, "Setting up HTTP01 challenge provider.")
		}
	}

	if tlsChallengePort != 0 {
		err := client.Challenge.SetTLSALPN01Provider(tlsalpn01.NewProviderServer("", strconv.Itoa(tlsChallengePort)))
		if err != nil {
			return nil, errors.Wrap(err, "Setting up TLSALPN01 challenge provider.")
		}
	}

	if dnsProvider != "" {
		provider, err := DNSProvider(dnsProvider)
		if err != nil {
			return nil, errors.Wrap(err, "Getting DNS01 challenge provider.")
		}
		err = client.Challenge.SetDNS01Provider(provider)
		if err != nil {
			return nil, errors.Wrap(err, "Setting up DNS01 challenge provider.")
		}
	}

	return client, nil
}

func (f *CertFetcher) FetchNewCert() ([]*x509.Certificate, error) {
	csr := certificate.ObtainForCSRRequest{
		CSR:    f.CertSignRequest,
		Bundle: true,
	}
	// Each resource comes back with the cert bytes, the bytes of the client's
	// private key, and a certificate URL.
	resource, err := f.legoClient.Certificate.ObtainForCSR(csr)
	if err != nil {
		return nil, err
	}

	if resource == nil {
		return nil, errors.New("No resource returned.")
	}

	if resource.Certificate == nil {
		return nil, errors.New("No certificates were returned.")
	}

	cert, err := signedexchange.ParseCertificates(resource.Certificate)
	if err != nil {
		return nil, err
	}

	return cert, err
}

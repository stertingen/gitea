// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package acme

import (
	"crypto"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ACME server response statuses used to describe Authorization and Challenge states.
const (
	StatusDeactivated = "deactivated"
	StatusInvalid     = "invalid"
	StatusPending     = "pending"
	StatusProcessing  = "processing"
	StatusRevoked     = "revoked"
	StatusUnknown     = "unknown"
	StatusValid       = "valid"
)

// CRLReasonCode identifies the reason for a certificate revocation.
type CRLReasonCode int

// CRL reason codes as defined in RFC 5280.
const (
	CRLReasonUnspecified          CRLReasonCode = 0
	CRLReasonKeyCompromise        CRLReasonCode = 1
	CRLReasonCACompromise         CRLReasonCode = 2
	CRLReasonAffiliationChanged   CRLReasonCode = 3
	CRLReasonSuperseded           CRLReasonCode = 4
	CRLReasonCessationOfOperation CRLReasonCode = 5
	CRLReasonCertificateHold      CRLReasonCode = 6
	CRLReasonRemoveFromCRL        CRLReasonCode = 8
	CRLReasonPrivilegeWithdrawn   CRLReasonCode = 9
	CRLReasonAACompromise         CRLReasonCode = 10
)

var (
	// ErrUnsupportedKey is returned when an unsupported key type is encountered.
	ErrUnsupportedKey = errors.New("acme: unknown key type; only RSA and ECDSA are supported")

	// ErrAccountAlreadyExists indicates that the Client's key has already been registered
	// with the CA. It is returned by Register method.
	ErrAccountAlreadyExists = errors.New("acme: account already exists")

	// ErrNoAccount indicates that the Client's key has not been registered with the CA.
	ErrNoAccount = errors.New("acme: account does not exist")
)

// Error is an ACME error, defined in Problem Details for HTTP APIs doc
// http://tools.ietf.org/html/draft-ietf-appsawg-http-problem.
type Error struct {
	// StatusCode is The HTTP status code generated by the origin server.
	StatusCode int
	// ProblemType is a URI reference that identifies the problem type,
	// typically in a "urn:acme:error:xxx" form.
	ProblemType string
	// Detail is a human-readable explanation specific to this occurrence of the problem.
	Detail string
	// Instance indicates a URL that the client should direct a human user to visit
	// in order for instructions on how to agree to the updated Terms of Service.
	// In such an event CA sets StatusCode to 403, ProblemType to
	// "urn:ietf:params:acme:error:userActionRequired" and a Link header with relation
	// "terms-of-service" containing the latest TOS URL.
	Instance string
	// Header is the original server error response headers.
	// It may be nil.
	Header http.Header
}

func (e *Error) Error() string {
	return fmt.Sprintf("%d %s: %s", e.StatusCode, e.ProblemType, e.Detail)
}

// AuthorizationError indicates that an authorization for an identifier
// did not succeed.
// It contains all errors from Challenge items of the failed Authorization.
type AuthorizationError struct {
	// URI uniquely identifies the failed Authorization.
	URI string

	// Identifier is an AuthzID.Value of the failed Authorization.
	Identifier string

	// Errors is a collection of non-nil error values of Challenge items
	// of the failed Authorization.
	Errors []error
}

func (a *AuthorizationError) Error() string {
	e := make([]string, len(a.Errors))
	for i, err := range a.Errors {
		e[i] = err.Error()
	}
	return fmt.Sprintf("acme: authorization error for %s: %s", a.Identifier, strings.Join(e, "; "))
}

// RateLimit reports whether err represents a rate limit error and
// any Retry-After duration returned by the server.
//
// See the following for more details on rate limiting:
// https://tools.ietf.org/html/draft-ietf-acme-acme-05#section-5.6
func RateLimit(err error) (time.Duration, bool) {
	e, ok := err.(*Error)
	if !ok {
		return 0, false
	}
	// Some CA implementations may return incorrect values.
	// Use case-insensitive comparison.
	if !strings.HasSuffix(strings.ToLower(e.ProblemType), ":ratelimited") {
		return 0, false
	}
	if e.Header == nil {
		return 0, true
	}
	return retryAfter(e.Header.Get("Retry-After")), true
}

// Account is a user account. It is associated with a private key.
// Non-RFC8555 fields are empty when interfacing with a compliant CA.
type Account struct {
	// URI is the account unique ID, which is also a URL used to retrieve
	// account data from the CA.
	// When interfacing with RFC8555-compliant CAs, URI is the "kid" field
	// value in JWS signed requests.
	URI string

	// Contact is a slice of contact info used during registration.
	// See https://tools.ietf.org/html/rfc8555#section-7.3 for supported
	// formats.
	Contact []string

	// Status indicates current account status as returned by the CA.
	// Possible values are "valid", "deactivated", and "revoked".
	Status string

	// OrdersURL is a URL from which a list of orders submitted by this account
	// can be fetched.
	OrdersURL string

	// The terms user has agreed to.
	// A value not matching CurrentTerms indicates that the user hasn't agreed
	// to the actual Terms of Service of the CA.
	//
	// It is non-RFC8555 compliant. Package users can store the ToS they agree to
	// during Client's Register call in the prompt callback function.
	AgreedTerms string

	// Actual terms of a CA.
	//
	// It is non-RFC8555 compliant. Use Directory's Terms field.
	// When a CA updates their terms and requires an account agreement,
	// a URL at which instructions to do so is available in Error's Instance field.
	CurrentTerms string

	// Authz is the authorization URL used to initiate a new authz flow.
	//
	// It is non-RFC8555 compliant. Use Directory's AuthzURL or OrderURL.
	Authz string

	// Authorizations is a URI from which a list of authorizations
	// granted to this account can be fetched via a GET request.
	//
	// It is non-RFC8555 compliant and is obsoleted by OrdersURL.
	Authorizations string

	// Certificates is a URI from which a list of certificates
	// issued for this account can be fetched via a GET request.
	//
	// It is non-RFC8555 compliant and is obsoleted by OrdersURL.
	Certificates string
}

// Directory is ACME server discovery data.
// See https://tools.ietf.org/html/rfc8555#section-7.1.1 for more details.
type Directory struct {
	// NonceURL indicates an endpoint where to fetch fresh nonce values from.
	NonceURL string

	// RegURL is an account endpoint URL, allowing for creating new accounts.
	// Pre-RFC8555 CAs also allow modifying existing accounts at this URL.
	RegURL string

	// OrderURL is used to initiate the certificate issuance flow
	// as described in RFC8555.
	OrderURL string

	// AuthzURL is used to initiate identifier pre-authorization flow.
	// Empty string indicates the flow is unsupported by the CA.
	AuthzURL string

	// CertURL is a new certificate issuance endpoint URL.
	// It is non-RFC8555 compliant and is obsoleted by OrderURL.
	CertURL string

	// RevokeURL is used to initiate a certificate revocation flow.
	RevokeURL string

	// KeyChangeURL allows to perform account key rollover flow.
	KeyChangeURL string

	// Term is a URI identifying the current terms of service.
	Terms string

	// Website is an HTTP or HTTPS URL locating a website
	// providing more information about the ACME server.
	Website string

	// CAA consists of lowercase hostname elements, which the ACME server
	// recognises as referring to itself for the purposes of CAA record validation
	// as defined in RFC6844.
	CAA []string

	// ExternalAccountRequired indicates that the CA requires for all account-related
	// requests to include external account binding information.
	ExternalAccountRequired bool
}

// Challenge encodes a returned CA challenge.
// Its Error field may be non-nil if the challenge is part of an Authorization
// with StatusInvalid.
type Challenge struct {
	// Type is the challenge type, e.g. "http-01", "tls-sni-02", "dns-01".
	Type string

	// URI is where a challenge response can be posted to.
	URI string

	// Token is a random value that uniquely identifies the challenge.
	Token string

	// Status identifies the status of this challenge.
	Status string

	// Error indicates the reason for an authorization failure
	// when this challenge was used.
	// The type of a non-nil value is *Error.
	Error error
}

// Authorization encodes an authorization response.
type Authorization struct {
	// URI uniquely identifies a authorization.
	URI string

	// Status identifies the status of an authorization.
	Status string

	// Identifier is what the account is authorized to represent.
	Identifier AuthzID

	// Challenges that the client needs to fulfill in order to prove possession
	// of the identifier (for pending authorizations).
	// For final authorizations, the challenges that were used.
	Challenges []*Challenge

	// A collection of sets of challenges, each of which would be sufficient
	// to prove possession of the identifier.
	// Clients must complete a set of challenges that covers at least one set.
	// Challenges are identified by their indices in the challenges array.
	// If this field is empty, the client needs to complete all challenges.
	Combinations [][]int
}

// AuthzID is an identifier that an account is authorized to represent.
type AuthzID struct {
	Type  string // The type of identifier, e.g. "dns".
	Value string // The identifier itself, e.g. "example.org".
}

// wireAuthz is ACME JSON representation of Authorization objects.
type wireAuthz struct {
	Status       string
	Challenges   []wireChallenge
	Combinations [][]int
	Identifier   struct {
		Type  string
		Value string
	}
}

func (z *wireAuthz) authorization(uri string) *Authorization {
	a := &Authorization{
		URI:          uri,
		Status:       z.Status,
		Identifier:   AuthzID{Type: z.Identifier.Type, Value: z.Identifier.Value},
		Combinations: z.Combinations, // shallow copy
		Challenges:   make([]*Challenge, len(z.Challenges)),
	}
	for i, v := range z.Challenges {
		a.Challenges[i] = v.challenge()
	}
	return a
}

func (z *wireAuthz) error(uri string) *AuthorizationError {
	err := &AuthorizationError{
		URI:        uri,
		Identifier: z.Identifier.Value,
	}
	for _, raw := range z.Challenges {
		if raw.Error != nil {
			err.Errors = append(err.Errors, raw.Error.error(nil))
		}
	}
	return err
}

// wireChallenge is ACME JSON challenge representation.
type wireChallenge struct {
	URI    string `json:"uri"`
	Type   string
	Token  string
	Status string
	Error  *wireError
}

func (c *wireChallenge) challenge() *Challenge {
	v := &Challenge{
		URI:    c.URI,
		Type:   c.Type,
		Token:  c.Token,
		Status: c.Status,
	}
	if v.Status == "" {
		v.Status = StatusPending
	}
	if c.Error != nil {
		v.Error = c.Error.error(nil)
	}
	return v
}

// wireError is a subset of fields of the Problem Details object
// as described in https://tools.ietf.org/html/rfc7807#section-3.1.
type wireError struct {
	Status   int
	Type     string
	Detail   string
	Instance string
}

func (e *wireError) error(h http.Header) *Error {
	return &Error{
		StatusCode:  e.Status,
		ProblemType: e.Type,
		Detail:      e.Detail,
		Instance:    e.Instance,
		Header:      h,
	}
}

// CertOption is an optional argument type for the TLS ChallengeCert methods for
// customizing a temporary certificate for TLS-based challenges.
type CertOption interface {
	privateCertOpt()
}

// WithKey creates an option holding a private/public key pair.
// The private part signs a certificate, and the public part represents the signee.
func WithKey(key crypto.Signer) CertOption {
	return &certOptKey{key}
}

type certOptKey struct {
	key crypto.Signer
}

func (*certOptKey) privateCertOpt() {}

// WithTemplate creates an option for specifying a certificate template.
// See x509.CreateCertificate for template usage details.
//
// In TLS ChallengeCert methods, the template is also used as parent,
// resulting in a self-signed certificate.
// The DNSNames field of t is always overwritten for tls-sni challenge certs.
func WithTemplate(t *x509.Certificate) CertOption {
	return (*certOptTemplate)(t)
}

type certOptTemplate x509.Certificate

func (*certOptTemplate) privateCertOpt() {}

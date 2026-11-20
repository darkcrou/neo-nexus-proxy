// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// BearerSource supplies a usable access token. Rejected is the token the
// upstream just rejected, or empty for ordinary lookup.
type BearerSource interface {
	Token(ctx context.Context, rejected string) (string, error)
}

type renewableBearer interface {
	BearerSource() BearerSource
}

func BearerSourceFor(p Provider) BearerSource {
	if provider, ok := p.(renewableBearer); ok {
		return provider.BearerSource()
	}
	return nil
}

var ErrBearerRejected = errors.New("upstream rejected the renewed credential")

// CallWithBearer retries one unauthorized request. send builds a fresh request
// on each call, and onReject can account for outgoing attempts before they are closed.
func CallWithBearer(ctx context.Context, source BearerSource, retried *bool, send func(string) (*http.Response, error), onReject func(*http.Response)) (*http.Response, error) {
	token, err := source.Token(ctx, "")
	if err != nil {
		return nil, err
	}
	for {
		resp, err := send(token)
		if err != nil || resp.StatusCode != http.StatusUnauthorized {
			return resp, err
		}
		if onReject != nil {
			onReject(resp)
		}
		resp.Body.Close()
		if *retried {
			return nil, ErrBearerRejected
		}
		*retried = true
		token, err = source.Token(ctx, token)
		if err != nil {
			return nil, err
		}
	}
}

// DoRenewableRequest only sends renewable credentials to their provider origin.
// Refusing redirects also prevents a redirect from resending a chat payload.
func DoRenewableRequest(client *http.Client, baseURL string, req *http.Request) (*http.Response, error) {
	origin, err := url.Parse(baseURL)
	if err != nil || origin.Scheme != "https" || req.URL.Scheme != origin.Scheme ||
		req.URL.Host != origin.Host || req.URL.User != nil {
		return nil, errors.New("renewable bearer request has an invalid destination")
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "NEXUS")
	}
	noRedirects := *client
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("provider redirected a renewable bearer request")
	}
	resp, err := noRedirects.Do(req)
	return resp, withoutURL(err)
}

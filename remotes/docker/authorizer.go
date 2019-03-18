/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context/ctxhttp"
)

var defaultExpiredIn = 60

type dockerAuthorizer struct {
	credentials func(string) (string, string, error)

	client *http.Client

	tokenLocalCache     *tokenCache
	challengeLocalCache *challengeCache
}

// NewAuthorizer creates a Docker authorizer using the provided function to
// get credentials for the token server or basic auth.
func NewAuthorizer(client *http.Client, f func(string) (string, string, error)) Authorizer {
	if client == nil {
		client = http.DefaultClient
	}

	return &dockerAuthorizer{
		credentials: f,
		client:      client,

		tokenLocalCache:     newTokenCache(),
		challengeLocalCache: newChallengeCache(),
	}
}

// Authorize handles auth request.
func (a *dockerAuthorizer) Authorize(ctx context.Context, req *http.Request) error {
	authID := authIDFromContext(ctx)

	// skip auth if there is no challenge
	//
	// FIXME(fuweid):
	// Basically, we don't know which kind of token will used for each
	// request. All the requests will be rejected at the first time.
	c, exist := a.challengeLocalCache.pop(authID)
	if !exist {
		return nil
	}

	host := req.URL.Host

	var (
		auth string
		err  error
	)

	switch s := c.scheme; s {
	case basicAuth:
		auth, err = a.doBasicAuth(ctx, host, c)
		if err != nil {
			return err
		}
	case bearerAuth:
		to, err := a.generateTokenOptions(ctx, host, c)
		if err != nil {
			return err
		}

		auth, err = a.doBearerAuth(ctx, to)
		if err != nil {
			return err
		}
	default:
		return errors.Wrap(errdefs.ErrNotImplemented, "failed to find supported auth scheme")
	}

	req.Header.Set("Authorization", auth)
	return nil
}

func (a *dockerAuthorizer) AddResponses(ctx context.Context, responses []*http.Response) error {
	last := responses[len(responses)-1]

	// don't cache challenge if there is no auth ID.
	authID := authIDFromContext(ctx)
	for _, c := range parseAuthHeader(last.Header) {
		if c.scheme == bearerAuth {
			if err := invalidAuthorization(c, responses); err != nil {
				return err
			}

			if authID != "" {
				a.challengeLocalCache.set(authID, c)
			}
			return nil
		} else if c.scheme == basicAuth && a.credentials != nil {
			if authID != "" {
				a.challengeLocalCache.set(authID, c)
			}
			return nil
		}
	}
	return errors.Wrap(errdefs.ErrNotImplemented, "failed to find supported auth scheme")
}

func (a *dockerAuthorizer) doBasicAuth(ctx context.Context, host string, c challenge) (string, error) {
	var (
		username, secret string
		err              error
	)

	if a.credentials != nil {
		username, secret, err = a.credentials(host)
		if err != nil {
			return "", err
		}
	}

	if username == "" || secret == "" {
		return "", fmt.Errorf("failed to handle basic auth because missing username or secret")
	}

	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + secret))
	return fmt.Sprintf("%s %s", c.scheme, auth), nil
}

func (a *dockerAuthorizer) doBearerAuth(ctx context.Context, to tokenOptions) (string, error) {
	tidx := tokenOptionsToTokenIndex(to)

	// check cache
	if value, exist := a.tokenLocalCache.lookup(tidx); exist {
		return fmt.Sprintf("%s %s", "Bearer", value), nil
	}

	var (
		t   token
		err error
	)

	if to.secret != "" {
		// credential information is provided, use oauth POST endpoint
		t, err = a.fetchTokenWithOAuth(ctx, to)
		if err != nil {
			return "", errors.Wrap(err, "failed to fetch oauth token")
		}
	} else {
		// do request anonymously
		t, err = a.fetchToken(ctx, to)
		if err != nil {
			return "", errors.Wrap(err, "failed to fetch anonymous token")
		}
	}

	a.tokenLocalCache.store(tidx, t)
	return fmt.Sprintf("%s %s", "Bearer", t.value), nil
}

func (a *dockerAuthorizer) generateTokenOptions(ctx context.Context, host string, c challenge) (tokenOptions, error) {
	realm, ok := c.parameters["realm"]
	if !ok {
		return tokenOptions{}, errors.New("no realm specified for token auth challenge")
	}

	realmURL, err := url.Parse(realm)
	if err != nil {
		return tokenOptions{}, errors.Wrap(err, "invalid token auth challenge realm")
	}

	to := tokenOptions{
		realm:   realmURL.String(),
		service: c.parameters["service"],
	}

	to.scopes = getTokenScopes(ctx, c.parameters)
	if len(to.scopes) == 0 {
		return tokenOptions{}, errors.Errorf("no scope specified for token auth challenge")
	}

	if a.credentials != nil {
		to.username, to.secret, err = a.credentials(host)
		if err != nil {
			return tokenOptions{}, err
		}
	}
	return to, nil
}

type tokenOptions struct {
	realm    string
	service  string
	scopes   []string
	username string
	secret   string
}

type postTokenResponse struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresIn    int       `json:"expires_in"`
	IssuedAt     time.Time `json:"issued_at"`
	Scope        string    `json:"scope"`
}

func (a *dockerAuthorizer) fetchTokenWithOAuth(ctx context.Context, to tokenOptions) (token, error) {
	form := url.Values{}
	form.Set("scope", strings.Join(to.scopes, " "))
	form.Set("service", to.service)
	// TODO: Allow setting client_id
	form.Set("client_id", "containerd-client")

	if to.username == "" {
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", to.secret)
	} else {
		form.Set("grant_type", "password")
		form.Set("username", to.username)
		form.Set("password", to.secret)
	}

	issuedAt := time.Now()
	resp, err := ctxhttp.Post(
		ctx, a.client, to.realm,
		"application/x-www-form-urlencoded; charset=utf-8",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return token{}, err
	}
	defer resp.Body.Close()

	// Registries without support for POST may return 404 for POST /v2/token.
	// As of September 2017, GCR is known to return 404.
	// As of February 2018, JFrog Artifactory is known to return 401.
	if (resp.StatusCode == 405 && to.username != "") || resp.StatusCode == 404 || resp.StatusCode == 401 {
		return a.fetchToken(ctx, to)
	} else if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		b, _ := ioutil.ReadAll(io.LimitReader(resp.Body, 64000)) // 64KB
		log.G(ctx).WithFields(logrus.Fields{
			"status": resp.Status,
			"body":   string(b),
		}).Debugf("token request failed")
		// TODO: handle error body and write debug output
		return token{}, errors.Errorf("unexpected status: %s", resp.Status)
	}

	decoder := json.NewDecoder(resp.Body)

	var tr postTokenResponse
	if err = decoder.Decode(&tr); err != nil {
		return token{}, fmt.Errorf("unable to decode token response: %s", err)
	}

	if tr.IssuedAt.IsZero() {
		tr.IssuedAt = issuedAt
	}

	if tr.ExpiresIn == 0 {
		tr.ExpiresIn = defaultExpiredIn
	}

	return token{
		value:     tr.AccessToken,
		expiredAt: tr.IssuedAt.Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

type getTokenResponse struct {
	Token        string    `json:"token"`
	AccessToken  string    `json:"access_token"`
	ExpiresIn    int       `json:"expires_in"`
	IssuedAt     time.Time `json:"issued_at"`
	RefreshToken string    `json:"refresh_token"`
}

// fetchToken fetches a token using a GET request
func (a *dockerAuthorizer) fetchToken(ctx context.Context, to tokenOptions) (token, error) {
	req, err := http.NewRequest("GET", to.realm, nil)
	if err != nil {
		return token{}, err
	}

	reqParams := req.URL.Query()

	if to.service != "" {
		reqParams.Add("service", to.service)
	}

	for _, scope := range to.scopes {
		reqParams.Add("scope", scope)
	}

	if to.secret != "" {
		req.SetBasicAuth(to.username, to.secret)
	}

	req.URL.RawQuery = reqParams.Encode()

	issuedAt := time.Now()
	resp, err := ctxhttp.Do(ctx, a.client, req)
	if err != nil {
		return token{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		// TODO: handle error body and write debug output
		return token{}, errors.Errorf("unexpected status: %s", resp.Status)
	}

	decoder := json.NewDecoder(resp.Body)

	var tr getTokenResponse
	if err = decoder.Decode(&tr); err != nil {
		return token{}, fmt.Errorf("unable to decode token response: %s", err)
	}

	// `access_token` is equivalent to `token` and if both are specified
	// the choice is undefined.  Canonicalize `access_token` by sticking
	// things in `token`.
	if tr.AccessToken != "" {
		tr.Token = tr.AccessToken
	}

	if tr.Token == "" {
		return token{}, ErrNoToken
	}

	if tr.IssuedAt.IsZero() {
		tr.IssuedAt = issuedAt
	}

	if tr.ExpiresIn == 0 {
		tr.ExpiresIn = defaultExpiredIn
	}

	return token{
		value:     tr.Token,
		expiredAt: tr.IssuedAt.Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

func invalidAuthorization(c challenge, responses []*http.Response) error {
	errStr := c.parameters["error"]
	if errStr == "" {
		return nil
	}

	n := len(responses)
	if n == 1 || (n > 1 && !sameRequest(responses[n-2].Request, responses[n-1].Request)) {
		return nil
	}

	return errors.Wrapf(ErrInvalidAuthorization, "server message: %s", errStr)
}

func sameRequest(r1, r2 *http.Request) bool {
	if r1.Method != r2.Method {
		return false
	}
	if *r1.URL != *r2.URL {
		return false
	}
	return true
}

func tokenOptionsToTokenIndex(to tokenOptions) tokenIndex {
	return tokenIndex{
		realm:    to.realm,
		service:  to.service,
		scopes:   strings.Join(to.scopes, " "),
		username: to.username,
		secret:   to.secret,
	}
}

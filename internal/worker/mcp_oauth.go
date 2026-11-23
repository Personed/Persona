package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// OAuthState is a managed connection's OAuth state, stored encrypted by the
// service. Field names match what the TypeScript SDK persisted, so stored
// credentials carry over.
type OAuthState struct {
	Client    json.RawMessage `json:"client,omitempty"`
	Tokens    *OAuthTokens    `json:"tokens,omitempty"`
	Verifier  string          `json:"verifier,omitempty"`
	Discovery *OAuthDiscovery `json:"discovery,omitempty"`
}

// OAuthTokens is an RFC 6749 token response.
type OAuthTokens struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
}

// OAuthDiscovery caches RFC 9728 and RFC 8414 discovery.
type OAuthDiscovery struct {
	AuthorizationServerURL      string                             `json:"authorizationServerUrl"`
	AuthorizationServerMetadata *oauthex.AuthServerMeta            `json:"authorizationServerMetadata,omitempty"`
	ResourceMetadata            *oauthex.ProtectedResourceMetadata `json:"resourceMetadata,omitempty"`
	ResourceMetadataURL         string                             `json:"resourceMetadataUrl,omitempty"`
}

type oauthClient struct {
	ClientID                string `json:"client_id"`
	ClientSecret            string `json:"client_secret,omitempty"`
	TokenEndpointAuthMethod string `json:"token_endpoint_auth_method,omitempty"`
}

func (s *OAuthState) clone() *OAuthState {
	out := &OAuthState{}
	if s != nil {
		*out = *s
		if s.Tokens != nil {
			tokens := *s.Tokens
			out.Tokens = &tokens
		}
	}
	return out
}

func oauthHTTPClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// authorizeMCP runs one half of the authorization code flow with PKCE: authorize
// refreshes the tokens or returns the authorization URL for the service's state;
// complete exchanges the code the redirect delivered.
func authorizeMCP(ctx context.Context, input MCPInput) (*MCPResult, error) {
	config := input.Config
	if config == nil || config.Transport != "http" || input.RedirectURL == "" || input.State == "" {
		return nil, codeError("EINVAL", "the authorization configuration is incomplete", "")
	}
	client := oauthHTTPClient()
	state := input.OAuth.clone()
	discovery, err := discover(ctx, config.URL, state.Discovery, client)
	if err != nil {
		return nil, err
	}
	state.Discovery = discovery
	endpoint := endpoints(discovery)
	if len(state.Client) == 0 {
		if input.Action == "complete" {
			return nil, codeError("EINVAL", "the authorization request expired", "")
		}
		if endpoint.RegistrationEndpoint == "" {
			return nil, codeError("EINVAL", "the authorization server does not support client registration", "")
		}
		registered, err := oauthex.RegisterClient(ctx, endpoint.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{
			ClientName:              "Persona",
			RedirectURIs:            []string{input.RedirectURL},
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
		}, client)
		if err != nil {
			return nil, err
		}
		if state.Client, err = json.Marshal(registered); err != nil {
			return nil, err
		}
	}
	conf, err := oauthConfig(state, input.RedirectURL)
	if err != nil {
		return nil, err
	}
	var resource []oauth2.AuthCodeOption
	if discovery.ResourceMetadata != nil && discovery.ResourceMetadata.Resource != "" {
		resource = append(resource, oauth2.SetAuthURLParam("resource", discovery.ResourceMetadata.Resource))
	}
	exchange := context.WithValue(ctx, oauth2.HTTPClient, client)
	if input.Action == "complete" {
		if state.Verifier == "" {
			return nil, codeError("EINVAL", "the authorization request expired", "")
		}
		token, err := conf.Exchange(exchange, input.AuthorizationCode, append(resource, oauth2.VerifierOption(state.Verifier))...)
		if err != nil {
			return nil, err
		}
		state.Tokens, state.Verifier = tokensFrom(token, ""), ""
		return &MCPResult{OAuth: state}, nil
	}
	if state.Tokens != nil && state.Tokens.RefreshToken != "" {
		if token, err := conf.TokenSource(exchange, &oauth2.Token{RefreshToken: state.Tokens.RefreshToken}).Token(); err == nil {
			state.Tokens = tokensFrom(token, state.Tokens.RefreshToken)
			return &MCPResult{OAuth: state}, nil
		}
	}
	if discovery.ResourceMetadata != nil {
		conf.Scopes = discovery.ResourceMetadata.ScopesSupported
	}
	verifier := oauth2.GenerateVerifier()
	options := append(resource, oauth2.S256ChallengeOption(verifier))
	for _, scope := range conf.Scopes {
		if scope == "offline_access" {
			options = append(options, oauth2.SetAuthURLParam("prompt", "consent"))
		}
	}
	state.Verifier = verifier
	return &MCPResult{AuthorizationURL: conf.AuthCodeURL(input.State, options...), OAuth: state}, nil
}

// discover finds the protected resource metadata at the endpoint's path, then at
// the root, and the authorization server's metadata. Without resource metadata
// the server's origin is the authorization server.
func discover(ctx context.Context, serverURL string, cached *OAuthDiscovery, client *http.Client) (*OAuthDiscovery, error) {
	if cached != nil && cached.AuthorizationServerURL != "" {
		return cached, nil
	}
	server, err := url.Parse(serverURL)
	if err != nil || server.Host == "" {
		return nil, codeError("EINVAL", "invalid MCP server URL", "")
	}
	origin := (&url.URL{Scheme: server.Scheme, Host: server.Host}).String()
	discovery := &OAuthDiscovery{AuthorizationServerURL: origin}
	type candidate struct{ metadata, resource string }
	var candidates []candidate
	if path := strings.TrimLeft(server.Path, "/"); path != "" {
		candidates = append(candidates, candidate{origin + "/.well-known/oauth-protected-resource/" + path, serverURL})
	}
	candidates = append(candidates,
		candidate{origin + "/.well-known/oauth-protected-resource", serverURL},
		candidate{origin + "/.well-known/oauth-protected-resource", origin})
	for _, c := range candidates {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, c.metadata, c.resource, client)
		if err != nil || prm == nil {
			continue
		}
		discovery.ResourceMetadata, discovery.ResourceMetadataURL = prm, c.metadata
		if len(prm.AuthorizationServers) > 0 {
			discovery.AuthorizationServerURL = prm.AuthorizationServers[0]
		}
		break
	}
	if meta, err := auth.GetAuthServerMetadata(ctx, discovery.AuthorizationServerURL, client); err == nil && meta != nil {
		discovery.AuthorizationServerMetadata = meta
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return discovery, nil
}

// endpoints falls back to the 2025-03-26 default paths without metadata.
func endpoints(discovery *OAuthDiscovery) oauthex.AuthServerMeta {
	if discovery.AuthorizationServerMetadata != nil {
		return *discovery.AuthorizationServerMetadata
	}
	base, _ := url.Parse(discovery.AuthorizationServerURL)
	at := func(path string) string { return base.ResolveReference(&url.URL{Path: path}).String() }
	return oauthex.AuthServerMeta{AuthorizationEndpoint: at("/authorize"), TokenEndpoint: at("/token"), RegistrationEndpoint: at("/register")}
}

func oauthConfig(state *OAuthState, redirectURL string) (*oauth2.Config, error) {
	var client oauthClient
	if err := json.Unmarshal(state.Client, &client); err != nil || client.ClientID == "" {
		return nil, codeError("EINVAL", "the OAuth client is invalid", "")
	}
	style := oauth2.AuthStyleAutoDetect
	switch {
	case client.TokenEndpointAuthMethod == "client_secret_basic":
		style = oauth2.AuthStyleInHeader
	case client.TokenEndpointAuthMethod == "client_secret_post", client.ClientSecret == "":
		style = oauth2.AuthStyleInParams
	}
	endpoint := endpoints(state.Discovery)
	return &oauth2.Config{
		ClientID:     client.ClientID,
		ClientSecret: client.ClientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     oauth2.Endpoint{AuthURL: endpoint.AuthorizationEndpoint, TokenURL: endpoint.TokenEndpoint, AuthStyle: style},
	}, nil
}

func tokensFrom(token *oauth2.Token, refresh string) *OAuthTokens {
	out := &OAuthTokens{AccessToken: token.AccessToken, TokenType: token.TokenType, ExpiresIn: token.ExpiresIn, RefreshToken: token.RefreshToken}
	if out.RefreshToken == "" {
		out.RefreshToken = refresh
	}
	if scope, ok := token.Extra("scope").(string); ok {
		out.Scope = scope
	}
	if id, ok := token.Extra("id_token").(string); ok {
		out.IDToken = id
	}
	return out
}

// oauthSource authorizes a connection with stored tokens. A rejected token is
// refreshed once per rejection; otherwise the connection needs sign-in again.
type oauthSource struct {
	mu    sync.Mutex
	state *OAuthState
	conf  *oauth2.Config
}

func newOAuthSource(state *OAuthState, redirectURL string) *oauthSource {
	source := &oauthSource{state: state.clone()}
	if source.state.Discovery != nil && len(source.state.Client) > 0 {
		source.conf, _ = oauthConfig(source.state, redirectURL)
	}
	return source
}

func (o *oauthSource) accessToken() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.state.Tokens == nil {
		return ""
	}
	return o.state.Tokens.AccessToken
}

func (o *oauthSource) TokenSource(context.Context) (oauth2.TokenSource, error) {
	token := o.accessToken()
	if token == "" {
		return nil, nil
	}
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token, TokenType: "Bearer"}), nil
}

func (o *oauthSource) Authorize(ctx context.Context, _ *http.Request, response *http.Response) error {
	_ = response.Body.Close()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.conf == nil || o.state.Tokens == nil || o.state.Tokens.RefreshToken == "" {
		return errUnauthorized
	}
	refresh := o.state.Tokens.RefreshToken
	token, err := o.conf.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, oauthHTTPClient()), &oauth2.Token{RefreshToken: refresh}).Token()
	if err != nil {
		var retrieve *oauth2.RetrieveError
		if errors.As(err, &retrieve) || ctx.Err() == nil {
			return errUnauthorized
		}
		return err
	}
	o.state.Tokens = tokensFrom(token, refresh)
	return nil
}

func (o *oauthSource) snapshot() *OAuthState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.state.clone()
}

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Device is a paired client device.
type Device struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Scope   string `json:"scope"`
	Created int64  `json:"created"`
	Expires int64  `json:"expires"`
}

// Info identifies a Persona service.
type Info struct {
	Product string `json:"product"`
	Version string `json:"version"`
}

// Info reads the service identity from /hosting/info.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var info Info
	err := c.getJSON(ctx, "/hosting/info", &info)
	if err == nil && info.Product != "persona" {
		err = fmt.Errorf("%s is not a Persona service", c.BaseURL)
	}
	return info, err
}

// Session reads the device that the client token belongs to.
func (c *Client) Session(ctx context.Context) (Device, error) {
	var body struct {
		Device Device `json:"device"`
	}
	err := c.getJSON(ctx, "/hosting/session", &body)
	return body.Device, err
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL(path), nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	res, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return readError(res)
	}
	return json.NewDecoder(res.Body).Decode(v)
}

// Pair exchanges a one-time pairing code for a device session and stores its token in c.Token.
func (c *Client) Pair(ctx context.Context, code, label string) (Device, error) {
	body, err := json.Marshal(map[string]string{"code": code, "label": label})
	if err != nil {
		return Device{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL("/hosting/exchange"), bytes.NewReader(body))
	if err != nil {
		return Device{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	res, err := c.httpClient().Do(req)
	if err != nil {
		return Device{}, err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return Device{}, readError(res)
	}
	var out struct {
		Device Device `json:"device"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return Device{}, fmt.Errorf("pairing: decode response: %w", err)
	}
	for _, cookie := range res.Cookies() {
		if cookie.Name == SessionCookie && cookie.Value != "" {
			c.Token = cookie.Value
			return out.Device, nil
		}
	}
	return Device{}, fmt.Errorf("pairing: response has no %s cookie", SessionCookie)
}

// Logout revokes the device session of the client token.
func (c *Client) Logout(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL("/hosting/logout"), nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	res, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return readError(res)
	}
	c.Token = ""
	return nil
}

package doclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const apiBase = "https://api.digitalocean.com/v2"

type Client struct {
	Token string
	HTTP  *http.Client
}

type Droplet struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	Region    Region   `json:"region"`
	SizeSlug  string   `json:"size_slug"`
	Networks  Networks `json:"networks"`
	CreatedAt string   `json:"created_at"`
	Tags      []string `json:"tags"`
}

type Region struct {
	Slug string `json:"slug"`
}

type Networks struct {
	V4 []NetworkV4 `json:"v4"`
}

type NetworkV4 struct {
	IPAddress string `json:"ip_address"`
	Type      string `json:"type"`
}

type SSHKey struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
}

type Size struct {
	Slug         string   `json:"slug"`
	Description  string   `json:"description"`
	Memory       int64    `json:"memory"`
	VCPUs        int64    `json:"vcpus"`
	Disk         int64    `json:"disk"`
	PriceHourly  float64  `json:"price_hourly"`
	PriceMonthly float64  `json:"price_monthly"`
	Regions      []string `json:"regions"`
	Available    bool     `json:"available"`
}

type CreateDropletRequest struct {
	Name       string   `json:"name"`
	Region     string   `json:"region"`
	Size       string   `json:"size"`
	Image      string   `json:"image"`
	SSHKeys    []string `json:"ssh_keys"`
	Monitoring bool     `json:"monitoring"`
	Tags       []string `json:"tags"`
	UserData   string   `json:"user_data"`
}

func New(token string) *Client {
	return &Client{
		Token: strings.TrimSpace(token),
		HTTP:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) ListDropletsByTag(ctx context.Context, tag string) ([]Droplet, error) {
	var resp struct {
		Droplets []Droplet `json:"droplets"`
	}
	if err := c.request(ctx, http.MethodGet, "/droplets?tag_name="+tag, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Droplets, nil
}

func (c *Client) CreateDroplet(ctx context.Context, req CreateDropletRequest) (*Droplet, error) {
	var resp struct {
		Droplet Droplet `json:"droplet"`
	}
	if err := c.request(ctx, http.MethodPost, "/droplets", req, &resp); err != nil {
		return nil, err
	}
	return &resp.Droplet, nil
}

func (c *Client) DeleteDroplet(ctx context.Context, id int64) error {
	return c.request(ctx, http.MethodDelete, fmt.Sprintf("/droplets/%d", id), nil, nil)
}

func (c *Client) ListSSHKeys(ctx context.Context) ([]SSHKey, error) {
	var all []SSHKey
	path := "/account/keys?per_page=200"
	for path != "" {
		var resp struct {
			Keys  []SSHKey `json:"ssh_keys"`
			Links struct {
				Pages struct {
					Next string `json:"next"`
				} `json:"pages"`
			} `json:"links"`
		}
		if err := c.request(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Keys...)
		path = strings.TrimPrefix(resp.Links.Pages.Next, apiBase)
	}
	return all, nil
}

func (c *Client) ListSizes(ctx context.Context) ([]Size, error) {
	var all []Size
	path := "/sizes?per_page=200"
	for path != "" {
		var resp struct {
			Sizes []Size `json:"sizes"`
			Links struct {
				Pages struct {
					Next string `json:"next"`
				} `json:"pages"`
			} `json:"links"`
		}
		if err := c.request(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Sizes...)
		path = strings.TrimPrefix(resp.Links.Pages.Next, apiBase)
	}
	return all, nil
}

func (c *Client) request(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
		reader = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (d Droplet) PublicIPv4() string {
	for _, n := range d.Networks.V4 {
		if n.Type == "public" {
			return n.IPAddress
		}
	}
	return ""
}

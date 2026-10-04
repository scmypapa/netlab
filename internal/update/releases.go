package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"netlab.local/core/api"
)

type Config struct {
	Repository string
	Token      string
	InstallDir string
	DataDir    string
	Version    string
}

type release struct {
	Tag         string     `json:"tag_name"`
	Name        string     `json:"name"`
	Body        string     `json:"body"`
	URL         string     `json:"html_url"`
	PublishedAt time.Time  `json:"published_at"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	Assets      []artifact `json:"assets"`
}

type artifact struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type github struct {
	cfg     Config
	baseURL string
	client  *http.Client
}

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)[.](0|[1-9][0-9]*)$`)

func newGitHub(cfg Config) github {
	return github{cfg: cfg, baseURL: "https://api.github.com", client: &http.Client{Timeout: 15 * time.Minute}}
}

func (g github) get(ctx context.Context, address, accept string) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Accept", accept)
	r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	r.Header.Set("User-Agent", "Netlab/"+g.cfg.Version)
	if g.cfg.Token != "" {
		r.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	}
	response, err := g.client.Do(r)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("GitHub 返回 %d；请核对发布包、仓库访问权限和 API 额度", response.StatusCode)
	}
	return response, nil
}

func (g github) release(ctx context.Context, version string) (release, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	path := "/releases/latest"
	if version != "" {
		path = "/releases/tags/" + url.PathEscape(version)
	}
	response, err := g.get(ctx, g.baseURL+"/repos/"+g.cfg.Repository+path, "application/vnd.github+json")
	if err != nil {
		return release{}, err
	}
	defer response.Body.Close()
	var value release
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&value); err != nil {
		return value, err
	}
	if _, err := versionParts(value.Tag); err != nil {
		return value, err
	}
	if value.Draft || value.Prerelease {
		return value, fmt.Errorf("%s 不是正式发布版本", value.Tag)
	}
	return value, nil
}

func (r release) public() api.UpdateRelease {
	return api.UpdateRelease{Version: r.Tag, Name: r.Name, Notes: r.Body, PublishedAt: r.PublishedAt, Url: r.URL}
}

func versionParts(value string) ([3]uint64, error) {
	var parts [3]uint64
	match := versionPattern.FindStringSubmatch(value)
	if len(match) != 4 {
		return parts, fmt.Errorf("发布版本应为 v主版本.次版本.修订号")
	}
	for i := range parts {
		part, err := strconv.ParseUint(match[i+1], 10, 64)
		if err != nil {
			return parts, err
		}
		parts[i] = part
	}
	return parts, nil
}

func newer(candidate, current string) bool {
	a, err := versionParts(candidate)
	if err != nil {
		return false
	}
	b, err := versionParts(current)
	if err != nil {
		return current == "dev"
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

package skills

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

var ErrNoRemoteSnapshot = errors.New("no remote skill snapshot")

type Client struct {
	Server     string
	AuthToken  string
	HTTPClient *http.Client
}

type planResponse struct {
	Missing []string `json:"missing"`
}

type apiResponse[T any] struct {
	Data T `json:"data"`
}

func (client Client) Head() (Snapshot, error) {
	var snapshot Snapshot
	status, err := client.doJSON(http.MethodGet, "/v1/skills/head", nil, &snapshot)
	if status == http.StatusNotFound {
		return Snapshot{}, ErrNoRemoteSnapshot
	}
	return snapshot, err
}

func (client Client) Plan(hashes []string) ([]string, error) {
	var result planResponse
	_, err := client.doJSON(http.MethodPost, "/v1/skills/plan", map[string]any{"hashes": hashes}, &result)
	return result.Missing, err
}

func (client Client) Upload(hash string, archive []byte) error {
	if err := validateArchiveHash(archive, hash); err != nil {
		return err
	}
	req, err := client.request(http.MethodPut, "/v1/skills/blobs/"+url.PathEscape(hash), bytes.NewReader(archive))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := client.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError(resp)
	}
	return nil
}

func (client Client) Commit(snapshot Snapshot) (Snapshot, error) {
	var committed Snapshot
	_, err := client.doJSON(http.MethodPost, "/v1/skills/commits", snapshot, &committed)
	return committed, err
}

func (client Client) Download(hash string) ([]byte, error) {
	if err := ValidateHash(hash); err != nil {
		return nil, err
	}
	req, err := client.request(http.MethodGet, "/v1/skills/blobs/"+url.PathEscape(hash), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, responseError(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxPackageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxPackageBytes {
		return nil, fmt.Errorf("remote skill exceeds %d bytes", MaxPackageBytes)
	}
	if err := validateArchiveHash(raw, hash); err != nil {
		return nil, err
	}
	return raw, nil
}

func (client Client) doJSON(method string, path string, payload any, target any) (int, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := client.request(method, path, body)
	if err != nil {
		return 0, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, responseError(resp)
	}
	if target == nil {
		return resp.StatusCode, nil
	}
	var wrapper apiResponse[json.RawMessage]
	if err := json.NewDecoder(resp.Body).Decode(&wrapper); err != nil {
		return resp.StatusCode, err
	}
	if err := json.Unmarshal(wrapper.Data, target); err != nil {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

func (client Client) request(method string, path string, body io.Reader) (*http.Request, error) {
	server := strings.TrimRight(strings.TrimSpace(client.Server), "/")
	if server == "" {
		return nil, fmt.Errorf("server URL is required")
	}
	req, err := http.NewRequest(method, server+path, body)
	if err != nil {
		return nil, err
	}
	if token := strings.TrimSpace(client.AuthToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

func (client Client) httpClient() *http.Client {
	if client.HTTPClient != nil {
		return client.HTTPClient
	}
	return &http.Client{Timeout: 45 * time.Second}
}

func responseError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	message := strings.TrimSpace(string(raw))
	if message == "" {
		message = resp.Status
	}
	return fmt.Errorf("server returned %s: %s", resp.Status, message)
}

func MergePreservingRemote(remote Snapshot, local Snapshot) Snapshot {
	byHash := map[string]Manifest{}
	localBindings := map[string]bool{}
	for _, skill := range local.Skills {
		for _, binding := range skill.Bindings {
			localBindings[BindingKey(binding)] = true
		}
	}
	for _, skill := range remote.Skills {
		kept := skill
		kept.Bindings = kept.Bindings[:0]
		for _, binding := range skill.Bindings {
			if !localBindings[BindingKey(binding)] {
				kept.Bindings = append(kept.Bindings, binding)
			}
		}
		if len(kept.Bindings) > 0 {
			byHash[kept.Hash] = kept
		}
	}
	for _, skill := range local.Skills {
		if existing, ok := byHash[skill.Hash]; ok {
			for _, binding := range skill.Bindings {
				existing.Bindings = appendBinding(existing.Bindings, binding)
			}
			byHash[skill.Hash] = existing
		} else {
			byHash[skill.Hash] = skill
		}
	}
	merged := Snapshot{
		ParentRevision: remote.Revision,
		DeviceID:       local.DeviceID,
		Skills:         make([]Manifest, 0, len(byHash)),
	}
	for _, skill := range byHash {
		merged.Skills = append(merged.Skills, skill)
	}
	SortSnapshot(&merged)
	return merged
}

func SameContent(a Snapshot, b Snapshot) bool {
	left := append([]Manifest(nil), a.Skills...)
	right := append([]Manifest(nil), b.Skills...)
	a.Skills = left
	b.Skills = right
	SortSnapshot(&a)
	SortSnapshot(&b)
	if len(a.Skills) != len(b.Skills) {
		return false
	}
	for i := range a.Skills {
		if a.Skills[i].Name != b.Skills[i].Name || a.Skills[i].Hash != b.Skills[i].Hash || a.Skills[i].Size != b.Skills[i].Size {
			return false
		}
		if len(a.Skills[i].Bindings) != len(b.Skills[i].Bindings) {
			return false
		}
		for j := range a.Skills[i].Bindings {
			if a.Skills[i].Bindings[j] != b.Skills[i].Bindings[j] {
				return false
			}
		}
	}
	return true
}

func Hashes(snapshot Snapshot) []string {
	seen := map[string]bool{}
	values := make([]string, 0, len(snapshot.Skills))
	for _, skill := range snapshot.Skills {
		if !seen[skill.Hash] {
			seen[skill.Hash] = true
			values = append(values, skill.Hash)
		}
	}
	sort.Strings(values)
	return values
}

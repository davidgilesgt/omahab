package scm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// WoodpeckerConfig holds configuration for the Woodpecker HTTP client.
type WoodpeckerConfig struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

type woodpeckerClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewWoodpeckerClient creates a WoodpeckerClient over the Woodpecker REST API.
func NewWoodpeckerClient(cfg WoodpeckerConfig) WoodpeckerClient {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	base = strings.TrimSuffix(base, "/api")
	hc := cfg.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	return &woodpeckerClient{
		baseURL:    base,
		token:      cfg.Token,
		httpClient: hc,
	}
}

func (c *woodpeckerClient) do(ctx context.Context, method, path string, reqBody any, respBody any) error {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	u := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.mapError(resp.StatusCode, data)
	}
	if respBody != nil && len(data) > 0 {
		if err := json.Unmarshal(data, respBody); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (c *woodpeckerClient) mapError(code int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(body) > 0 {
		var m struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &m) == nil && strings.TrimSpace(m.Message) != "" {
			msg = m.Message
		}
	}
	if msg == "" {
		msg = http.StatusText(code)
	}
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: woodpecker unauthorized (%d): %s", ErrValidation, code, msg)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrConflict, msg)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %s", ErrValidation, msg)
	default:
		if code >= 400 && code < 500 {
			return fmt.Errorf("%w: woodpecker client error %d: %s", ErrValidation, code, msg)
		}
		return fmt.Errorf("woodpecker server error %d: %s", code, msg)
	}
}

type woodpeckerRepoJSON struct {
	ID            int64  `json:"id"`
	ForgeRemoteID any    `json:"forge_remote_id"`
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Active        bool   `json:"active"`
	Config        string `json:"config_file"`
	Trusted       *struct {
		Network  bool `json:"network"`
		Volumes  bool `json:"volumes"`
		Security bool `json:"security"`
	} `json:"trusted"`
}

func (j *woodpeckerRepoJSON) toCIRepo() *CIRepo {
	var fid int64
	switch v := j.ForgeRemoteID.(type) {
	case string:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			fid = n
		}
	case float64:
		fid = int64(v)
	case int64:
		fid = v
	case int:
		fid = int64(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			fid = n
		}
	}
	owner := j.Owner
	name := j.Name
	if owner == "" && j.FullName != "" {
		if idx := strings.Index(j.FullName, "/"); idx > 0 {
			owner = j.FullName[:idx]
			name = j.FullName[idx+1:]
		}
	}
	trusted := false
	if j.Trusted != nil {
		trusted = j.Trusted.Network || j.Trusted.Volumes || j.Trusted.Security
	}
	cfg := j.Config
	if cfg == "" {
		cfg = ".woodpecker.yaml"
	}
	return &CIRepo{
		ID:              j.ID,
		ForgejoRemoteID: fid,
		Owner:           owner,
		Name:            name,
		Active:          j.Active,
		Trusted:         trusted,
		PipelinePath:    cfg,
	}
}

func (c *woodpeckerClient) EnsureRepo(ctx context.Context, in EnsureCIRepoInput) (*CIRepo, error) {
	if in.ForgejoRemoteID == 0 {
		return nil, fmt.Errorf("%w: forge_remote_id required", ErrValidation)
	}
	if strings.TrimSpace(in.Owner) == "" || strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("%w: owner and name required", ErrValidation)
	}
	path := fmt.Sprintf("/api/repos?forge_remote_id=%s", url.QueryEscape(strconv.FormatInt(in.ForgejoRemoteID, 10)))
	var out woodpeckerRepoJSON
	if err := c.do(ctx, http.MethodPost, path, nil, &out); err != nil {
		return nil, err
	}
	// Patch trusted/config if needed.
	needsPatch := false
	patch := map[string]any{}
	if in.PipelinePath != "" && in.PipelinePath != ".woodpecker.yaml" {
		needsPatch = true
		patch["config_file"] = in.PipelinePath
	}
	if in.Trusted {
		needsPatch = true
		patch["trusted"] = map[string]bool{"network": true, "volumes": true, "security": true}
	}
	if needsPatch && out.ID != 0 {
		patchPath := fmt.Sprintf("/api/repos/%d", out.ID)
		_ = c.do(ctx, http.MethodPatch, patchPath, patch, &out)
	}
	return out.toCIRepo(), nil
}

func (c *woodpeckerClient) DeactivateRepo(ctx context.Context, repoID int64) error {
	if repoID == 0 {
		return fmt.Errorf("%w: repo id required", ErrValidation)
	}
	path := fmt.Sprintf("/api/repos/%d", repoID)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

type woodpeckerPipelineJSON struct {
	ID       int64  `json:"id"`
	Number   int64  `json:"number"`
	Status   string `json:"status"`
	Branch   string `json:"branch"`
	Commit   string `json:"commit"`
	Event    string `json:"event"`
	Message  string `json:"message"`
	Author   string `json:"author"`
	Started  int64  `json:"started"`
	Finished int64  `json:"finished"`
	Workflows []*struct {
		Name     string `json:"name"`
		Children []*struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			PID  int    `json:"pid"`
		} `json:"children"`
	} `json:"workflows"`
}

func (j *woodpeckerPipelineJSON) toRun() *Run {
	status := j.Status
	if status == "" {
		status = "pending"
	}
	started := ""
	if j.Started != 0 {
		started = strconv.FormatInt(j.Started, 10)
	}
	finished := ""
	if j.Finished != 0 {
		finished = strconv.FormatInt(j.Finished, 10)
	}
	return &Run{
		Number:       int(j.Number),
		WoodpeckerID: j.ID,
		Status:       status,
		Branch:       j.Branch,
		CommitSHA:    j.Commit,
		Event:        j.Event,
		Message:      j.Message,
		Author:       j.Author,
		StartedAt:    started,
		FinishedAt:   finished,
	}
}

func (c *woodpeckerClient) ListRuns(ctx context.Context, repoID int64, limit int) ([]*Run, error) {
	if repoID == 0 {
		return nil, fmt.Errorf("%w: repo id required", ErrValidation)
	}
	path := fmt.Sprintf("/api/repos/%d/pipelines", repoID)
	if limit > 0 {
		path += fmt.Sprintf("?perPage=%d&page=1", limit)
	}
	var out []*woodpeckerPipelineJSON
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	runs := make([]*Run, 0, len(out))
	for _, j := range out {
		runs = append(runs, j.toRun())
	}
	return runs, nil
}

func (c *woodpeckerClient) GetRun(ctx context.Context, repoID int64, number int) (*Run, error) {
	if repoID == 0 {
		return nil, fmt.Errorf("%w: repo id required", ErrValidation)
	}
	if number <= 0 {
		return nil, fmt.Errorf("%w: pipeline number must be positive", ErrValidation)
	}
	path := fmt.Sprintf("/api/repos/%d/pipelines/%d", repoID, number)
	var out woodpeckerPipelineJSON
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.toRun(), nil
}

func (c *woodpeckerClient) LogRefs(ctx context.Context, repoID int64, number int) ([]*LogRef, error) {
	if repoID == 0 {
		return nil, fmt.Errorf("%w: repo id required", ErrValidation)
	}
	if number <= 0 {
		return nil, fmt.Errorf("%w: pipeline number must be positive", ErrValidation)
	}
	path := fmt.Sprintf("/api/repos/%d/pipelines/%d", repoID, number)
	var out woodpeckerPipelineJSON
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	var refs []*LogRef
	for _, wf := range out.Workflows {
		for _, step := range wf.Children {
			refs = append(refs, &LogRef{
				StepID:   step.ID,
				StepName: step.Name,
				LogID:    fmt.Sprintf("%d-%d", number, step.ID),
				URL:      fmt.Sprintf("%s/api/repos/%d/logs/%d/%d", strings.TrimRight(c.baseURL, "/"), repoID, number, step.ID),
			})
		}
	}
	return refs, nil
}

func (c *woodpeckerClient) Rerun(ctx context.Context, repoID int64, number int) error {
	if repoID == 0 {
		return fmt.Errorf("%w: repo id required", ErrValidation)
	}
	if number <= 0 {
		return fmt.Errorf("%w: pipeline number must be positive", ErrValidation)
	}
	path := fmt.Sprintf("/api/repos/%d/pipelines/%d", repoID, number)
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

func (c *woodpeckerClient) Cancel(ctx context.Context, repoID int64, number int) error {
	if repoID == 0 {
		return fmt.Errorf("%w: repo id required", ErrValidation)
	}
	if number <= 0 {
		return fmt.Errorf("%w: pipeline number must be positive", ErrValidation)
	}
	path := fmt.Sprintf("/api/repos/%d/pipelines/%d/cancel", repoID, number)
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

var _ WoodpeckerClient = (*woodpeckerClient)(nil)

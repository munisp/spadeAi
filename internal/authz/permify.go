// Package authz enforces relationship-based authorization through Permify (fail closed).
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const Schema = `
entity user {}

entity organization {
  relation owner @user
  relation member @user
  permission manage = owner
  permission view = owner or member
}

entity project {
  relation org @organization
  relation owner @user
  relation admin @user
  relation operator @user
  relation viewer @user
  relation billing @user
  permission manage = owner or admin
  permission create_service = owner
  permission operate = owner or admin or operator
  permission view = owner or admin or operator or viewer or billing
  permission view_billing = owner or billing
}

entity service {
  relation project @project
  permission view = project.view
  permission operate = project.operate
  permission destroy = project.manage
}
`

type Client struct {
	BaseURL string
	Tenant  string
	HTTP    *http.Client
}

func New(baseURL string) *Client {
	return &Client{BaseURL: baseURL, Tenant: "t1", HTTP: &http.Client{Timeout: 5 * time.Second}}
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/tenants/"+c.Tenant+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("permify unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("permify %s: HTTP %d %v", path, resp.StatusCode, e)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) WriteSchema(ctx context.Context) error {
	return c.post(ctx, "/schemas/write", map[string]any{"schema": Schema}, nil)
}

type Tuple struct {
	EntityType, EntityID, Relation, SubjectType, SubjectID string
}

func (c *Client) Write(ctx context.Context, tuples ...Tuple) error {
	ts := make([]map[string]any, 0, len(tuples))
	for _, t := range tuples {
		ts = append(ts, map[string]any{
			"entity":   map[string]string{"type": t.EntityType, "id": t.EntityID},
			"relation": t.Relation,
			"subject":  map[string]string{"type": t.SubjectType, "id": t.SubjectID},
		})
	}
	return c.post(ctx, "/data/write", map[string]any{"metadata": map[string]any{"schema_version": ""}, "tuples": ts}, nil)
}

func (c *Client) Delete(ctx context.Context, t Tuple) error {
	return c.post(ctx, "/data/delete", map[string]any{
		"tuple_filter": map[string]any{
			"entity":   map[string]any{"type": t.EntityType, "ids": []string{t.EntityID}},
			"relation": t.Relation,
			"subject":  map[string]any{"type": t.SubjectType, "ids": []string{t.SubjectID}},
		},
		"attribute_filter": map[string]any{},
	}, nil)
}

// Check returns (false, err) on any failure so callers deny by default.
func (c *Client) Check(ctx context.Context, entityType, entityID, permission, userID string) (bool, error) {
	var out struct {
		Can string `json:"can"`
	}
	err := c.post(ctx, "/permissions/check", map[string]any{
		"metadata":   map[string]any{"schema_version": "", "snap_token": "", "depth": 20},
		"entity":     map[string]string{"type": entityType, "id": entityID},
		"permission": permission,
		"subject":    map[string]string{"type": "user", "id": userID},
	}, &out)
	if err != nil {
		return false, err
	}
	return out.Can == "CHECK_RESULT_ALLOWED", nil
}

func (c *Client) Healthy(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/healthz", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("permify healthz HTTP %d", resp.StatusCode)
	}
	return nil
}

// T builds a relationship tuple: entity#relation@subject.
func T(entityType, entityID, relation, subjectType, subjectID string) Tuple {
	return Tuple{EntityType: entityType, EntityID: entityID, Relation: relation, SubjectType: subjectType, SubjectID: subjectID}
}

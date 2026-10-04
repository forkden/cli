package cloud

import "context"

// Resources lists physical lifecycle metadata in the selected workspace.
func (c *Client) Resources(ctx context.Context, orgID, projectID string) ([]Resource, error) {
	path, err := projectPath(orgID, projectID)
	if err != nil {
		return nil, err
	}
	var out []Resource
	if err := c.request(ctx, "GET", path+"/resources", nil, &out); err != nil {
		return nil, err
	}
	for _, r := range out {
		if !validResource(r, orgID, projectID) {
			return nil, failure("unavailable")
		}
	}
	return out, nil
}

// Versions lists all reserved capture numbers, including unsuccessful attempts.
func (c *Client) Versions(ctx context.Context, orgID, projectID, cloneID string) ([]CloneVersion, error) {
	path, err := projectPath(orgID, projectID)
	if err != nil {
		return nil, err
	}
	if !ValidID(cloneID) {
		return nil, failure("invalid_input")
	}
	var out []CloneVersion
	if err := c.request(ctx, "GET", path+"/resources/"+cloneID+"/versions", nil, &out); err != nil {
		return nil, err
	}
	previous := 0
	for _, v := range out {
		if v.CloneID != cloneID || v.Number <= previous || v.Number > 10000 || !ValidID(v.JobID) || v.CreatedAt.IsZero() {
			return nil, failure("unavailable")
		}
		switch v.Status {
		case "queued", "running", "cancelling", "ready", "failed", "cancelled", "needs_attention", "deleted":
		default:
			return nil, failure("unavailable")
		}
		previous = v.Number
	}
	return out, nil
}

func validResource(r Resource, orgID, projectID string) bool {
	if r.OrgID != orgID || r.ProjectID != projectID || !ValidID(r.ID) || !ValidID(r.ConnectorID) || !ValidID(r.ProfileID) || !ValidID(r.LastJobID) || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() {
		return false
	}
	switch r.Kind {
	case "clone":
		if r.CloneID != "" || r.CloneVersion != 0 || r.LatestVersion < 0 || r.LatestVersion > 10000 {
			return false
		}
	case "fork":
		if !ValidID(r.CloneID) || r.CloneVersion < 1 || r.CloneVersion > 10000 || r.LatestVersion != 0 {
			return false
		}
	default:
		return false
	}
	switch r.Status {
	case "creating", "ready", "refreshing", "closing", "deleting", "closed", "deleted", "failed", "cancelled", "needs_attention":
		return true
	default:
		return false
	}
}

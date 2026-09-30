package api

import (
	"encoding/json"
	"testing"
)

func TestRenderDowngradesEveryNestedObject(t *testing.T) {
	list := map[string]any{
		"object": "list",
		"data": []any{
			map[string]any{"object": "webhook_endpoint", "id": "we_1", "status": "disabled"},
			map[string]any{"object": "event", "related_object": map[string]any{
				"object": map[string]any{"object": "webhook_endpoint", "id": "we_2", "status": "enabled"},
			}},
		},
	}
	raw, err := render(list, "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Data []struct {
			Disabled      *bool   `json:"disabled"`
			Status        *string `json:"status"`
			RelatedObject struct {
				Object struct {
					Disabled *bool   `json:"disabled"`
					Status   *string `json:"status"`
				} `json:"object"`
			} `json:"related_object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	first, nested := got.Data[0], got.Data[1].RelatedObject.Object
	if first.Status != nil || first.Disabled == nil || !*first.Disabled {
		t.Errorf("top-level endpoint = %s", raw)
	}
	if nested.Status != nil || nested.Disabled == nil || *nested.Disabled {
		t.Errorf("nested endpoint = %s", raw)
	}
}

func TestRenderAtTheCurrentVersionIsPlainJSON(t *testing.T) {
	raw, err := render(map[string]any{"object": "webhook_endpoint", "status": "enabled", "created": int64(1 << 60)}, CurrentVersion)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"created":1152921504606846976,"object":"webhook_endpoint","status":"enabled"}` {
		t.Fatalf("render = %s", raw)
	}
}

func TestRenderKeepsLargeIntegersExact(t *testing.T) {
	raw, err := render(map[string]any{"object": "webhook_endpoint", "status": "enabled", "created": int64(1<<62 + 1)}, "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Created int64 }
	if err := json.Unmarshal(raw, &got); err != nil || got.Created != 1<<62+1 {
		t.Fatalf("created = %d, %v; want 4611686018427387905", got.Created, err)
	}
}

func TestEveryChangeNamesAKnownVersion(t *testing.T) {
	for _, c := range changes {
		if !knownVersion(c.version) || c.version == Versions[0] {
			t.Errorf("change %q is for version %s, which has no older version to downgrade to", c.description, c.version)
		}
	}
}

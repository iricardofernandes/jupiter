package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
)

// Versions are dates, oldest first. A merchant is pinned to the version current when it
// was created, and a request can name another in the Jupiter-Version header.
var Versions = []string{"2026-09-01", "2026-09-30"}

var CurrentVersion = Versions[len(Versions)-1]

// change is one breaking change: downgrade rewrites an object of the given type from
// the shape of version into the shape of the version before it.
type change struct {
	version     string
	object      string
	description string
	downgrade   func(map[string]any)
}

var changes = []change{
	{
		version:     "2026-09-30",
		object:      "webhook_endpoint",
		description: "The disabled boolean is replaced by status, enabled or disabled.",
		downgrade: func(o map[string]any) {
			status, ok := o["status"]
			if !ok {
				return
			}
			o["disabled"] = status == "disabled"
			delete(o, "status")
		},
	},
}

func knownVersion(v string) bool {
	return slices.Contains(Versions, v)
}

// render marshals v, which always has the current version's shape, and rewrites every
// object inside it back to the shape of version.
func render(v any, version string) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if version == CurrentVersion {
		return raw, nil
	}
	var tree any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("re-reading %T: %w", v, err)
	}
	for i := len(changes) - 1; i >= 0; i-- {
		if changes[i].version > version {
			apply(tree, changes[i])
		}
	}
	return json.Marshal(tree)
}

func apply(node any, c change) {
	switch n := node.(type) {
	case map[string]any:
		for _, child := range n {
			apply(child, c)
		}
		if n["object"] == c.object {
			c.downgrade(n)
		}
	case []any:
		for _, child := range n {
			apply(child, c)
		}
	}
}

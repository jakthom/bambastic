package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// Exercise the responses that hydrate an authenticated operator's screens on a
// representative installation. Use the pinned release schemas, including nested arrays
// and unions. Device-dependent requests have their own protocol fixtures.
func TestReadResponseContract(t *testing.T) {
	s := startTestSystem(t)
	actor := s.install()
	s.json("POST", "/api/v1/projects/", `{"name":"Fleet fixtures"}`, actor, 200)
	s.json("POST", "/api/v1/library/folders", `{"name":"Slicer fixtures"}`, actor, 200)
	s.json("POST", "/api/v1/inventory/spools", `{"material":"PLA","label_weight":1000,"cost_per_kg":21}`, actor, 200)
	s.json("POST", "/api/v1/api-keys", `{"name":"Read-only monitor","can_read_status":true}`, actor, 201)
	var upload bytes.Buffer
	form := multipart.NewWriter(&upload)
	file, err := form.CreateFormFile("file", "fixture.gcode")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("; fixture\nG1 X2\n")); err != nil {
		t.Fatal(err)
	}
	if err = form.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := s.call("POST", "/api/v1/archives/upload", form.FormDataContentType(), upload.String(), actor, 200)
	var archive struct {
		ID int64 `json:"id"`
	}
	if e := json.Unmarshal(raw, &archive); e != nil || archive.ID == 0 {
		t.Fatal("archive registration", e)
	}
	s.call("POST", "/api/v1/queue/", "application/json", fmt.Sprintf(`{"archive_id":%d,"plate_id":1,"target_model":"X1C"}`, archive.ID), actor, 200)

	var contract struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
		Paths map[string]struct {
			Get struct {
				Responses map[string]struct {
					Content map[string]struct {
						Schema json.RawMessage `json:"schema"`
					} `json:"content"`
				} `json:"responses"`
			} `json:"get"`
		} `json:"paths"`
	}
	local := bytes.ReplaceAll(reference, []byte("#/components/schemas/"), []byte("#/$defs/"))
	if e := json.Unmarshal(local, &contract); e != nil {
		t.Fatal(e)
	}
	sections := strings.Fields(`
auth/me auth/tokens auth/tokens/all auth/encryption-status auth/2fa/status auth/oidc/providers
auth/oidc/providers/all auth/oidc/links users/ users/slim groups/permissions groups/
printer-locations/ queue/ archives/ archives/last-per-printer archives/slim archives/stats
filament-catalog/ finance/me/balance finance/me/transactions finance/transactions
finance/cost-centers/mine finance/cost-centers inventory/catalog inventory/locations
inventory/suppliers inventory/colors inventory/spools inventory/profile-defaults
inventory/assignments inventory/stats/suppliers inventory/stats/material-numbers inventory/usage
inventory/sku-settings inventory/shopping-list label-templates settings/ cloud/status
orca-cloud/status local-presets/ smart-plugs/discover/status smart-plugs/discover/devices
ha-sensors/ location-ha-sensors/ notifications/ notifications/logs notifications/logs/stats
notification-templates/ notification-templates/variables user-notifications/preferences
spoolman/status sponsor-prompt/check maintenance/types maintenance/overview camwall/printers
external-links/ projects/ projects/templates library/folders/ library/files/ library/tags/
library/trash library/trash/settings slicer-pipelines/ pipeline-runs slicer/printer-models
slicer/loaded-spools archives/purge/settings makerworld/status makerworld/recent-imports
manyfold/config manyfold/status api-keys/ connect/apps support/debug-logging support/logs
discovery/info discovery/status discovery/printers discovery/scan/status pending-uploads/
github-backup/status github-backup/cloud-accounts github-backup/logs spoolbuddy/devices
`)
	properties := map[string]json.RawMessage{}
	values := map[string]any{}
	for _, section := range sections {
		path := "/api/v1/" + section
		response := contract.Paths[path].Get.Responses["200"].Content["application/json"].Schema
		if len(response) == 0 {
			t.Fatalf("no release response for %s", path)
		}
		properties[section] = response
		body, _ := s.call("GET", path, "", "", actor, 200)
		var value any
		if e := json.Unmarshal(body, &value); e != nil {
			t.Fatalf("%s: %v", path, e)
		}
		values[section] = value
	}
	combined, e := json.Marshal(map[string]any{"type": "object", "properties": properties, "$defs": contract.Components.Schemas})
	if e != nil {
		t.Fatal(e)
	}
	var schema jsonschema.Schema
	if e = json.Unmarshal(combined, &schema); e != nil {
		t.Fatal(e)
	}
	resolved, e := schema.Resolve(nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, section := range sections {
		if e = resolved.Validate(map[string]any{section: values[section]}); e != nil {
			t.Errorf("%s: %v", section, e)
		}
	}
}

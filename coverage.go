package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
)

type routeCoverage struct {
	Family     string   `json:"family"`
	Registered int      `json:"registered"`
	Reference  int      `json:"reference"`
	Missing    []string `json:"missing"`
}

func coverage(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("bambastic coverage", flag.ContinueOnError)
	format := flags.Bool("json", false, "Print machine-readable summary and missing routes")
	missing := flags.Bool("missing", false, "List individual missing operations")
	if e := flags.Parse(args); e != nil {
		return e
	}
	dir, e := os.MkdirTemp("", "bambastic-coverage-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(dir)
	app, e := createApp(dir)
	if e != nil {
		return e
	}
	defer app.close()
	schema := httptest.NewRecorder()
	app.handler.ServeHTTP(schema, httptest.NewRequest("GET", "/openapi.json", nil))
	if schema.Code != http.StatusOK {
		return fmt.Errorf("live OpenAPI returned %d", schema.Code)
	}
	var live, release struct {
		Paths map[string]map[string]json.RawMessage
	}
	if e = json.Unmarshal(schema.Body.Bytes(), &live); e != nil {
		return e
	}
	if e = json.Unmarshal(reference, &release); e != nil {
		return e
	}
	families := map[string]*routeCoverage{}
	total, implemented := 0, 0
	for path, m := range release.Paths {
		name, _, _ := strings.Cut(strings.TrimPrefix(path, "/api/v1/"), "/")
		if name == "" {
			name = "service"
		}
		f := families[name]
		if f == nil {
			f = &routeCoverage{Family: name, Missing: []string{}}
			families[name] = f
		}
		for method := range m {
			total++
			f.Reference++
			if _, ok := live.Paths[path][method]; ok {
				implemented++
				f.Registered++
			} else {
				f.Missing = append(f.Missing, strings.ToUpper(method)+" "+path)
			}
		}
	}
	rows := []*routeCoverage{}
	for _, f := range families {
		sort.Strings(f.Missing)
		rows = append(rows, f)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Family < rows[j].Family })
	if *format {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"baseline": "Bambuddy v.1.3.2", "registered": implemented, "reference": total, "families": rows})
	}
	fmt.Fprintf(out, "%d of %d reference operations have registered handlers.\n", implemented, total)
	for _, f := range rows {
		fmt.Fprintf(out, "%-26s %3d / %-3d\n", f.Family, f.Registered, f.Reference)
		if *missing {
			for _, op := range f.Missing {
				fmt.Fprintf(out, "  %s\n", op)
			}
		}
	}
	return nil
}

package production

import (
	"archive/zip"
	"encoding/xml"
	"html"
	"io"
	"path/filepath"
	"strings"
)

// IntakeDisplayName resolves the same name for a review card and its committed archive.
func IntakeDisplayName(file, name string, preferFilename bool) string {
	base := filepath.Base(name)
	for _, suffix := range []string{".gcode.3mf", ".3mf", ".gcode"} {
		if strings.HasSuffix(strings.ToLower(base), suffix) {
			base = base[:len(base)-len(suffix)]
			break
		}
	}
	if preferFilename {
		return base
	}
	z, e := zip.OpenReader(file)
	if e != nil {
		return base
	}
	defer z.Close()
	for _, entry := range z.File {
		if !strings.EqualFold(entry.Name, "3D/3dmodel.model") {
			continue
		}
		r, e := entry.Open()
		if e != nil {
			return base
		}
		defer r.Close()
		decoder := xml.NewDecoder(io.LimitReader(r, 1<<20))
		for {
			t, e := decoder.Token()
			if e != nil {
				return base
			}
			node, ok := t.(xml.StartElement)
			if !ok {
				continue
			}
			if node.Name.Local == "resources" || node.Name.Local == "build" {
				return base
			}
			if node.Name.Local != "metadata" {
				continue
			}
			for _, attr := range node.Attr {
				if attr.Name.Local == "name" && strings.EqualFold(attr.Value, "Title") {
					var title string
					if decoder.DecodeElement(&title, &node) == nil {
						for i := 0; i < 3; i++ {
							clean := html.UnescapeString(title)
							if clean == title {
								break
							}
							title = clean
						}
						title = strings.Join(strings.Fields(title), " ")
						if title != "" && len(title) <= 255 {
							return title
						}
					}
					return base
				}
			}
		}
	}
	return base
}

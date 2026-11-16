package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A utility class a template uses must have a rule: without one the element
// silently loses its border, spacing or colour and nothing else notices.
func TestTemplateUtilityClassesHaveRules(t *testing.T) {
	var css strings.Builder
	for _, f := range []string{"app.css", "utilities.css", "tokens.css"} {
		b, err := os.ReadFile(filepath.Join("static", "css", f))
		if err != nil {
			t.Fatal(err)
		}
		css.Write(b)
	}
	selectors := map[string]bool{}
	comment := regexp.MustCompile(`(?s)/\*.*?\*/`)
	for _, m := range regexp.MustCompile(`\.((?:\\.|[A-Za-z0-9_-])+)`).FindAllStringSubmatch(comment.ReplaceAllString(css.String(), ""), -1) {
		selectors[strings.ReplaceAll(m[1], `\`, "")] = true
	}

	// Class names built from utilities: spacing, sizing, flex/grid, colour, border and text.
	utility := regexp.MustCompile(`^(?:[a-z]+:)*-?(?:p[xytblr]?|m[xytblr]?|gap(?:-[xy])?|space-[xy]|w|h|min-[wh]|max-[wh]|text|bg|border(?:-[tblr])?|divide(?:-[xy])?|rounded|flex|grid|col-span|items|justify|self|order|overflow|opacity|font|leading|tracking|z|top|left|right|bottom|inset|ring)(?:-[\w./\[\]()%#,-]+)?$`)
	attr := regexp.MustCompile(`class="([^"]*)"`)
	var missing []string
	seen := map[string]bool{}
	err := filepath.WalkDir("templates", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		b, _ := os.ReadFile(path)
		for _, m := range attr.FindAllStringSubmatch(string(b), -1) {
			for _, tok := range strings.Fields(m[1]) {
				if strings.Contains(tok, "{{") || strings.Contains(tok, "}}") || seen[tok] || !utility.MatchString(tok) {
					continue
				}
				seen[tok] = true
				if !selectors[tok] {
					missing = append(missing, tok+" ("+filepath.Base(path)+")")
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("classes used in templates with no CSS rule:\n  %s", strings.Join(missing, "\n  "))
	}
}

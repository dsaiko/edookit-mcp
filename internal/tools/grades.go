package tools

import (
	"context"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/dsaiko/edookit-mcp/internal/client"
)

// Grade represents a single grade entry scraped from the grades page.
type Grade struct {
	Subject string `json:"subject"`
	Value   string `json:"value"`
	Date    string `json:"date,omitempty"`
	Note    string `json:"note,omitempty"`
}

// GetGrades is an illustrative tool implementation. Replace the selectors and
// URL path with the real ones once you inspect the target page in devtools.
func GetGrades(ctx context.Context, c *client.Client, period string) ([]Grade, error) {
	path := "/grades"
	if period != "" {
		path += "?period=" + period
	}

	doc, err := c.GetDoc(ctx, path)
	if err != nil {
		return nil, err
	}

	var out []Grade
	doc.Find("table.grades tbody tr").Each(func(_ int, row *goquery.Selection) {
		cells := row.Find("td")
		if cells.Length() < 2 {
			return
		}
		out = append(out, Grade{
			Subject: strings.TrimSpace(cells.Eq(0).Text()),
			Value:   strings.TrimSpace(cells.Eq(1).Text()),
			Date:    strings.TrimSpace(cells.Eq(2).Text()),
		})
	})
	return out, nil
}

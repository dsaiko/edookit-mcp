package tools

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"

	"github.com/dsaiko/edookit-mcp/internal/client"
)

// Edookit has no standalone "my classes" list; the authoritative source for a
// teacher's courses (and their student rosters, including split half-groups) is
// the Hodnocení → Známkování v tabulce page (`evaluation-grid`): the course
// dropdown enumerates the courses, and selecting one loads its students.
const (
	evaluationGridPath     = "/handler/page/evaluation-grid"
	evaluationGridDataPath = "/handler/grid/evaluation-grid-data"
	// myCoursesPGroup is the default "Pohled" (view) — the signed-in teacher's
	// own courses ("Moje kurzy").
	myCoursesPGroup = "my"
	// maxCourseStudentFetch bounds concurrent roster fetches when include_students
	// is set, so listing rosters for ~all courses doesn't open a request per
	// course all at once.
	maxCourseStudentFetch = 4
)

// Student is one pupil in a course roster.
type Student struct {
	StudyID string `json:"study_id"`
	Name    string `json:"name"`
	Class   string `json:"class,omitempty"`
}

// Course is one teaching course/group. SplitGroup is true for a half-group
// (e.g. "AUT 1 - 4SA"), which Edookit renders indented under its full course
// ("AUT - 4SA"). Students is populated only when requested.
type Course struct {
	CourseID   string    `json:"course_id"`
	Name       string    `json:"name"`
	SplitGroup bool      `json:"split_group"`
	Students   []Student `json:"students,omitempty"`
	// Error is set (only in include_students mode) when this course's roster
	// fetch failed, so the caller can tell a real empty roster from a failure —
	// the others still return their students.
	Error string `json:"error,omitempty"`
}

// CoursesOptions controls ListCourses.
type CoursesOptions struct {
	// CourseID, when set, returns just that one course with its students.
	CourseID string
	// IncludeStudents fetches every course's roster (heavier — one request per
	// course). Ignored when CourseID is set (that always includes students).
	IncludeStudents bool
}

// ListCourses returns the signed-in teacher's courses. By default it's the
// course list only (one request); with CourseID it returns that course plus its
// roster; with IncludeStudents it populates every course's roster.
func ListCourses(ctx context.Context, cli *client.Client, opts CoursesOptions) ([]Course, error) {
	courses, pgroup, err := fetchCourseList(ctx, cli)
	if err != nil {
		return nil, err
	}

	if opts.CourseID != "" {
		for i := range courses {
			if courses[i].CourseID == opts.CourseID {
				students, serr := courseStudents(ctx, cli, pgroup, opts.CourseID)
				if serr != nil {
					return nil, serr
				}
				courses[i].Students = students
				return []Course{courses[i]}, nil
			}
		}
		return nil, fmt.Errorf("course %q not found among %d courses (call edookit_list_courses without arguments to see valid course_id values)", opts.CourseID, len(courses))
	}

	if opts.IncludeStudents {
		fillAllRosters(ctx, cli, pgroup, courses)
		// A canceled/expired context makes every roster fetch fail; that's a
		// hard failure, not a silently-partial result.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return courses, nil
}

// fillAllRosters populates every course's roster with bounded concurrency. A
// course whose fetch fails gets its Error set (and is logged) rather than
// failing the whole listing, so a real empty roster stays distinguishable from
// a failure.
func fillAllRosters(ctx context.Context, cli *client.Client, pgroup string, courses []Course) {
	sem := make(chan struct{}, maxCourseStudentFetch)
	var wg sync.WaitGroup
	for i := range courses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			students, err := courseStudents(ctx, cli, pgroup, courses[i].CourseID)
			if err != nil {
				log.Printf("[tools] roster for course %s (%s) failed: %v", courses[i].CourseID, courses[i].Name, err)
				courses[i].Error = err.Error()
				return
			}
			courses[i].Students = students
		}(i)
	}
	wg.Wait()
}

// fetchCourseList loads the evaluation-grid page, parses the course dropdown
// (the `class_course` tied-select), and returns the courses plus the pgroup
// ("Pohled") id they came from — so roster fetches address the same view the
// list was read from.
func fetchCourseList(ctx context.Context, cli *client.Client) ([]Course, string, error) {
	var page map[string]any
	if err := cli.GetJSON(ctx, evaluationGridPath, &page); err != nil {
		return nil, "", fmt.Errorf("load evaluation-grid: %w", err)
	}
	node := findNamedNode(page, "class_course")
	if node == nil {
		return nil, "", fmt.Errorf("course selector not found on evaluation-grid (Edookit page shape may have changed)")
	}
	return parseCourseOptions(node)
}

// parseCourseOptions reads the course list out of a `class_course` tied-select
// node. Shape:
//
//	{"data": {"data": [ {"d": ["my", "-- Moje kurzy --"], "c": [
//	    {"d": ["__NULL__", "Vyberte kurz >>"]},
//	    {"d": ["label-…", "-- AKTIVNÍ KURZY ---"]},
//	    {"d": ["myc-…-20037", "AUT - 4SA"]},
//	    {"d": ["myc-…-20102", "  AUT 1 - 4SA"]},   // indent = split half-group
//	    …]}]}}
func parseCourseOptions(node map[string]any) ([]Course, string, error) {
	views, _ := asSlice(dig(node, "data", "data"))
	if len(views) == 0 {
		return nil, "", fmt.Errorf("course selector has no views")
	}
	opts, pgroup := courseOptionsForView(views, myCoursesPGroup)

	courses := make([]Course, 0, len(opts))
	for _, o := range opts {
		om, ok := asMap(o)
		if !ok {
			continue
		}
		d, _ := asSlice(om["d"])
		if len(d) < 2 {
			continue
		}
		id, _ := asStr(d[0])
		name, _ := asStr(d[1])
		if id == "" || id == "__NULL__" || strings.HasPrefix(id, "label-") {
			continue // placeholder / section header, not a real course
		}
		courses = append(courses, Course{
			CourseID:   id,
			Name:       strings.TrimSpace(name),
			SplitGroup: name != strings.TrimLeft(name, "  "), // leading indent marks a half-group
		})
	}
	return courses, pgroup, nil
}

// courseOptionsForView returns the option list ("c") of the preferred pgroup
// view (default "my") and the id of the view actually used. If the preferred
// view is absent it falls back to the first view and returns that view's id
// too, so roster fetches stay consistent with where the list came from.
func courseOptionsForView(views []any, preferred string) (opts []any, pgroup string) {
	var firstOpts []any
	var firstPgroup string
	haveFirst := false
	for _, v := range views {
		vm, ok := asMap(v)
		if !ok {
			continue
		}
		c, _ := asSlice(vm["c"])
		id := ""
		if d, _ := asSlice(vm["d"]); len(d) > 0 {
			id, _ = asStr(d[0])
		}
		if !haveFirst {
			firstOpts, firstPgroup, haveFirst = c, id, true
		}
		if id == preferred {
			return c, id
		}
	}
	return firstOpts, firstPgroup
}

// courseStudents loads one course's roster from the evaluation grid data, in
// the given pgroup ("Pohled") view.
func courseStudents(ctx context.Context, cli *client.Client, pgroup, courseID string) ([]Student, error) {
	q := url.Values{}
	q.Set("pgroup_id", pgroup)
	q.Set("course_id", courseID)

	var resp evalGridResponse
	if err := cli.GetJSON(ctx, evaluationGridDataPath+"?"+q.Encode(), &resp); err != nil {
		return nil, fmt.Errorf("load roster for %s: %w", courseID, err)
	}
	if len(resp.Components.Workspace) == 0 {
		return nil, nil
	}

	rows := resp.Components.Workspace[0].Data
	students := make([]Student, 0, len(rows))
	for _, row := range rows {
		if len(row) < 3 {
			continue
		}
		id, _ := asStr(row[0])
		cellHTML, _ := asStr(row[2])
		name, class := parseStudentCell(cellHTML)
		if name == "" {
			continue // header / aggregate row carries no student name
		}
		students = append(students, Student{StudyID: id, Name: name, Class: class})
	}
	return students, nil
}

// parseStudentCell extracts "Surname Forename" and the class from a roster
// cell like `<b><span>Baloušek Tomáš<small> (4SA)</small></span> …</b>`.
func parseStudentCell(cellHTML string) (name, class string) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader("<div>" + cellHTML + "</div>"))
	if err != nil {
		return "", ""
	}
	span := doc.Find("span").First()
	if span.Length() == 0 {
		return "", ""
	}
	if small := span.Find("small").First(); small.Length() > 0 {
		class = strings.Trim(strings.TrimSpace(small.Text()), "()")
		small.Remove() // so the span text below excludes the class suffix
	}
	name = strings.Join(strings.Fields(span.Text()), " ")
	return name, class
}

// evalGridResponse is the subset of /handler/grid/evaluation-grid-data we read.
// Rows are heterogeneous (ids, HTML, grade cells), so cells are decoded as any.
type evalGridResponse struct {
	Components struct {
		Workspace []struct {
			Data [][]any `json:"data"`
		} `json:"workspace"`
	} `json:"components"`
}

// --- small any-tree helpers (the page JSON is loosely typed) ---

func asMap(v any) (map[string]any, bool) { m, ok := v.(map[string]any); return m, ok }
func asSlice(v any) ([]any, bool)        { s, ok := v.([]any); return s, ok }
func asStr(v any) (string, bool)         { s, ok := v.(string); return s, ok }

// dig walks nested maps by key, returning nil if any step is missing.
func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := asMap(v)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// findNamedNode does a depth-first search for a map carrying "name": <name>.
func findNamedNode(v any, name string) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		if n, _ := asStr(t["name"]); n == name {
			return t
		}
		for _, child := range t {
			if got := findNamedNode(child, name); got != nil {
				return got
			}
		}
	case []any:
		for _, child := range t {
			if got := findNamedNode(child, name); got != nil {
				return got
			}
		}
	}
	return nil
}

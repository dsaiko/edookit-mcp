package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseStudentCell(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, wantName, wantClass string
	}{
		{"name + class", `<b><span>Baloušek Tomáš<small> (4SA)</small></span> <span>(2)</span></b>`, "Baloušek Tomáš", "4SA"},
		{"collapses whitespace", `<span>  Bauer   Jiří <small>(4SA)</small></span>`, "Bauer Jiří", "4SA"},
		{"no class small", `<span>Novák Jan</span>`, "Novák Jan", ""},
		{"empty header cell", `<b></b>`, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotName, gotClass := parseStudentCell(tc.in)
			if gotName != tc.wantName || gotClass != tc.wantClass {
				t.Errorf("parseStudentCell(%q) = (%q,%q), want (%q,%q)", tc.in, gotName, gotClass, tc.wantName, tc.wantClass)
			}
		})
	}
}

// coursesServer serves the two endpoints ListCourses uses: the evaluation-grid
// page (course dropdown) and the per-course roster grid.
func coursesServer(t *testing.T) *httptest.Server {
	t.Helper()
	const pageJSON = `{"authenticated":true,"components":{"panel":[
		{"name":"class_course","type":"tied_selects","data":{"data":[
			{"d":["my","-- Moje kurzy --",1],"c":[
				{"d":["__NULL__","Vyberte kurz >>",1]},
				{"d":["label-1-a","-- AKTIVNÍ KURZY ---"]},
				{"d":["myc-1-100","AUT - 4SA"]},
				{"d":["myc-1-101","  AUT 1 - 4SA"]},
				{"d":["myc-1-102","  AUT 2 - 4SA"]}
			]}
		]}}
	]}}`
	roster := map[string]string{
		"myc-1-100": `{"components":{"workspace":[{"data":[
			["hdr","hdr","<b></b>"],
			["19701","19701","<b><span>Baloušek Tomáš<small> (4SA)</small></span></b>"],
			["19464","19464","<b><span>Bauer Jiří<small> (4SA)</small></span></b>"]
		]}]}}`,
		"myc-1-101": `{"components":{"workspace":[{"data":[
			["hdr","hdr","<b></b>"],
			["19701","19701","<b><span>Baloušek Tomáš<small> (4SA)</small></span></b>"]
		]}]}}`,
		"myc-1-102": `{"components":{"workspace":[{"data":[
			["19464","19464","<b><span>Bauer Jiří<small> (4SA)</small></span></b>"]
		]}]}}`,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("warmup ok")) })
	mux.HandleFunc("/handler/page/evaluation-grid", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(pageJSON))
	})
	mux.HandleFunc("/handler/grid/evaluation-grid-data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, ok := roster[r.URL.Query().Get("course_id")]
		if !ok {
			body = `{"components":{"workspace":[{"data":[]}]}}`
		}
		_, _ = w.Write([]byte(body))
	})
	return httptest.NewServer(mux)
}

func TestListCourses_ListOnly(t *testing.T) {
	t.Parallel()
	srv := coursesServer(t)
	defer srv.Close()
	cli := buildClient(t, srv)

	courses, err := ListCourses(context.Background(), cli, CoursesOptions{})
	if err != nil {
		t.Fatalf("ListCourses: %v", err)
	}
	if len(courses) != 3 {
		t.Fatalf("got %d courses, want 3 (placeholder + label skipped)", len(courses))
	}
	want := []struct {
		id, name string
		split    bool
	}{
		{"myc-1-100", "AUT - 4SA", false},
		{"myc-1-101", "AUT 1 - 4SA", true},
		{"myc-1-102", "AUT 2 - 4SA", true},
	}
	for i, w := range want {
		c := courses[i]
		if c.CourseID != w.id || c.Name != w.name || c.SplitGroup != w.split {
			t.Errorf("course[%d] = %+v, want {%s %s split=%v}", i, c, w.id, w.name, w.split)
		}
		if c.Students != nil {
			t.Errorf("course[%d] should have no students in list-only mode", i)
		}
	}
}

func TestListCourses_DrillDownRoster(t *testing.T) {
	t.Parallel()
	srv := coursesServer(t)
	defer srv.Close()
	cli := buildClient(t, srv)

	got, err := ListCourses(context.Background(), cli, CoursesOptions{CourseID: "myc-1-101"})
	if err != nil {
		t.Fatalf("ListCourses: %v", err)
	}
	if len(got) != 1 || got[0].CourseID != "myc-1-101" {
		t.Fatalf("got %+v, want just course myc-1-101", got)
	}
	if len(got[0].Students) != 1 {
		t.Fatalf("got %d students, want 1 (header row skipped)", len(got[0].Students))
	}
	s := got[0].Students[0]
	if s.StudyID != "19701" || s.Name != "Baloušek Tomáš" || s.Class != "4SA" {
		t.Errorf("student = %+v, want {19701 Baloušek Tomáš 4SA}", s)
	}
}

func TestListCourses_IncludeStudents_PartialFailure(t *testing.T) {
	t.Parallel()
	const pageJSON = `{"authenticated":true,"components":{"panel":[
		{"name":"class_course","type":"tied_selects","data":{"data":[
			{"d":["my","-- Moje kurzy --"],"c":[
				{"d":["myc-1-100","AUT - 4SA"]},
				{"d":["myc-1-102","AUT 2 - 4SA"]}
			]}
		]}}
	]}}`
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("warmup ok")) })
	mux.HandleFunc("/handler/page/evaluation-grid", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(pageJSON))
	})
	mux.HandleFunc("/handler/grid/evaluation-grid-data", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("course_id") == "myc-1-102" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"components":{"workspace":[{"data":[["19701","19701","<b><span>Baloušek Tomáš<small> (4SA)</small></span></b>"]]}]}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cli := buildClient(t, srv)

	courses, err := ListCourses(context.Background(), cli, CoursesOptions{IncludeStudents: true})
	if err != nil {
		t.Fatalf("ListCourses: %v", err)
	}
	byID := map[string]Course{}
	for _, c := range courses {
		byID[c.CourseID] = c
	}
	if got := byID["myc-1-100"]; len(got.Students) != 1 || got.Error != "" {
		t.Errorf("good course = %+v, want 1 student and no error", got)
	}
	if got := byID["myc-1-102"]; got.Error == "" || len(got.Students) != 0 {
		t.Errorf("failed course = %+v, want Error set and no students (distinguishable from empty)", got)
	}
}

func TestListCourses_UnknownCourseID(t *testing.T) {
	t.Parallel()
	srv := coursesServer(t)
	defer srv.Close()
	cli := buildClient(t, srv)

	_, err := ListCourses(context.Background(), cli, CoursesOptions{CourseID: "myc-1-999"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want a not-found error", err)
	}
}

func TestListCourses_IncludeStudents(t *testing.T) {
	t.Parallel()
	srv := coursesServer(t)
	defer srv.Close()
	cli := buildClient(t, srv)

	courses, err := ListCourses(context.Background(), cli, CoursesOptions{IncludeStudents: true})
	if err != nil {
		t.Fatalf("ListCourses: %v", err)
	}
	got := map[string]int{}
	for _, c := range courses {
		got[c.CourseID] = len(c.Students)
	}
	want := map[string]int{"myc-1-100": 2, "myc-1-101": 1, "myc-1-102": 1}
	for id, n := range want {
		if got[id] != n {
			t.Errorf("course %s has %d students, want %d", id, got[id], n)
		}
	}
}

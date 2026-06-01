//! `list_courses` — the signed-in teacher's courses + student rosters. Port of
//! Go's `internal/tools/courses.go`.
//!
//! Edookit has no standalone "my classes" list; the authoritative source is the
//! Hodnocení → Známkování v tabulce page (`evaluation-grid`): the course
//! dropdown enumerates courses, and selecting one loads its students.

use std::sync::LazyLock;

use anyhow::{anyhow, bail};
use futures::stream::{self, StreamExt};
use scraper::Selector;
use serde::Serialize;
use serde_json::Value;

use super::htmlutil::{collapse_whitespace, parse_fragment, text_excluding};
use crate::client::Client;

const EVAL_GRID_PATH: &str = "/handler/page/evaluation-grid";
const EVAL_GRID_DATA_PATH: &str = "/handler/grid/evaluation-grid-data";
/// The default "Pohled" (view) — the signed-in teacher's own courses.
const MY_COURSES_PGROUP: &str = "my";
/// Bounds concurrent roster fetches when `include_students` is set.
const MAX_COURSE_STUDENT_FETCH: usize = 4;

#[derive(Debug, Clone, Default, PartialEq, Serialize)]
pub struct Student {
    pub study_id: String,
    pub name: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub class: String,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct Course {
    pub course_id: String,
    pub name: String,
    pub split_group: bool,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub students: Vec<Student>,
    /// Set (only in include_students mode) when this course's roster fetch
    /// failed, so a real empty roster stays distinguishable from a failure.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error: String,
}

#[derive(Debug, Default, Clone)]
pub struct CoursesOptions {
    pub course_id: String,
    pub include_students: bool,
}

/// Returns the signed-in teacher's courses. Default: course list only (one
/// request). With `course_id`: that course plus its roster. With
/// `include_students`: every course's roster (heavier).
pub async fn list_courses(cli: &Client, opts: CoursesOptions) -> anyhow::Result<Vec<Course>> {
    let (mut courses, pgroup) = fetch_course_list(cli).await?;

    if !opts.course_id.is_empty() {
        if let Some(i) = courses.iter().position(|c| c.course_id == opts.course_id) {
            let students = course_students(cli, &pgroup, &opts.course_id).await?;
            courses[i].students = students;
            return Ok(vec![courses.swap_remove(i)]);
        }
        bail!(
            "course {:?} not found among {} courses (call list_courses without arguments to see valid course_id values)",
            opts.course_id,
            courses.len()
        );
    }

    if opts.include_students {
        fill_all_rosters(cli, &pgroup, &mut courses).await;
    }
    Ok(courses)
}

/// Populates every course's roster with bounded concurrency. A course whose
/// fetch fails gets its `error` set (and is logged) rather than failing the
/// whole listing — `buffer_unordered` runs at most N at once on this task, so
/// no spawning / `Arc` is needed.
async fn fill_all_rosters(cli: &Client, pgroup: &str, courses: &mut [Course]) {
    let ids: Vec<(usize, String)> = courses
        .iter()
        .enumerate()
        .map(|(i, c)| (i, c.course_id.clone()))
        .collect();
    let results: Vec<(usize, anyhow::Result<Vec<Student>>)> = stream::iter(ids)
        .map(|(i, id)| async move { (i, course_students(cli, pgroup, &id).await) })
        .buffer_unordered(MAX_COURSE_STUDENT_FETCH)
        .collect()
        .await;
    for (i, res) in results {
        match res {
            Ok(students) => courses[i].students = students,
            Err(e) => {
                tracing::warn!(
                    "roster for course {} ({}) failed: {e}",
                    courses[i].course_id,
                    courses[i].name
                );
                courses[i].error = e.to_string();
            }
        }
    }
}

/// Loads the evaluation-grid page, parses the course dropdown (`class_course`),
/// and returns the courses plus the pgroup ("Pohled") id they came from.
async fn fetch_course_list(cli: &Client) -> anyhow::Result<(Vec<Course>, String)> {
    let page: Value = cli
        .get_json(EVAL_GRID_PATH)
        .await
        .map_err(|e| anyhow!("load evaluation-grid: {e}"))?;
    let node = find_named_node(&page, "class_course").ok_or_else(|| {
        anyhow!(
            "course selector not found on evaluation-grid (Edookit page shape may have changed)"
        )
    })?;
    parse_course_options(node)
}

/// Reads the course list out of a `class_course` tied-select node. Shape:
/// `{"data": {"data": [ {"d": ["my", "-- Moje kurzy --"], "c": [ {"d": [id, label]}, … ]} ]}}`
fn parse_course_options(node: &Value) -> anyhow::Result<(Vec<Course>, String)> {
    let views = node
        .get("data")
        .and_then(|d| d.get("data"))
        .and_then(Value::as_array)
        .filter(|v| !v.is_empty())
        .ok_or_else(|| anyhow!("course selector has no views"))?;

    let (opts, pgroup) = course_options_for_view(views, MY_COURSES_PGROUP);

    let mut courses = Vec::new();
    for o in opts {
        let Some(d) = o.get("d").and_then(Value::as_array) else {
            continue;
        };
        if d.len() < 2 {
            continue;
        }
        let id = d[0].as_str().unwrap_or("");
        let name = d[1].as_str().unwrap_or("");
        if id.is_empty() || id == "__NULL__" || id.starts_with("label-") {
            continue; // placeholder / section header
        }
        courses.push(Course {
            course_id: id.to_string(),
            name: name.trim().to_string(),
            split_group: name.starts_with(' '), // leading indent marks a half-group
            ..Default::default()
        });
    }
    Ok((courses, pgroup))
}

/// Returns the option list ("c") of the preferred pgroup view (default "my")
/// and the id of the view actually used; falls back to the first view.
fn course_options_for_view<'a>(views: &'a [Value], preferred: &str) -> (Vec<&'a Value>, String) {
    let mut first: Option<(Vec<&'a Value>, String)> = None;
    for v in views {
        let opts: Vec<&Value> = v
            .get("c")
            .and_then(Value::as_array)
            .map(|a| a.iter().collect())
            .unwrap_or_default();
        let id = v
            .get("d")
            .and_then(Value::as_array)
            .and_then(|d| d.first())
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string();
        if first.is_none() {
            first = Some((opts.clone(), id.clone()));
        }
        if id == preferred {
            return (opts, id);
        }
    }
    first.unwrap_or_default()
}

/// Loads one course's roster from the evaluation grid data.
async fn course_students(
    cli: &Client,
    pgroup: &str,
    course_id: &str,
) -> anyhow::Result<Vec<Student>> {
    let q = url::form_urlencoded::Serializer::new(String::new())
        .append_pair("pgroup_id", pgroup)
        .append_pair("course_id", course_id)
        .finish();
    let full = format!("{EVAL_GRID_DATA_PATH}?{q}");
    let resp: Value = cli
        .get_json(&full)
        .await
        .map_err(|e| anyhow!("load roster for {course_id}: {e}"))?;

    let Some(rows) = resp
        .get("components")
        .and_then(|c| c.get("workspace"))
        .and_then(Value::as_array)
        .and_then(|w| w.first())
        .and_then(|w| w.get("data"))
        .and_then(Value::as_array)
    else {
        return Ok(Vec::new());
    };

    let mut students = Vec::with_capacity(rows.len());
    for row in rows {
        let Some(cells) = row.as_array() else {
            continue;
        };
        if cells.len() < 3 {
            continue;
        }
        let id = cells[0].as_str().unwrap_or("");
        let cell_html = cells[2].as_str().unwrap_or("");
        let (name, class) = parse_student_cell(cell_html);
        if name.is_empty() {
            continue; // header / aggregate row carries no student name
        }
        students.push(Student {
            study_id: id.to_string(),
            name,
            class,
        });
    }
    Ok(students)
}

static SPAN_SEL: LazyLock<Selector> = LazyLock::new(|| Selector::parse("span").unwrap());
static SMALL_SEL: LazyLock<Selector> = LazyLock::new(|| Selector::parse("small").unwrap());

/// Extracts "Surname Forename" and the class from a roster cell like
/// `<b><span>Baloušek Tomáš<small> (4SA)</small></span> …</b>`.
fn parse_student_cell(cell_html: &str) -> (String, String) {
    let doc = parse_fragment(cell_html);
    let Some(span) = doc.select(&SPAN_SEL).next() else {
        return (String::new(), String::new());
    };
    let class = span
        .select(&SMALL_SEL)
        .next()
        .map(|s| {
            s.text()
                .collect::<String>()
                .trim()
                .trim_matches(['(', ')'])
                .trim()
                .to_string()
        })
        .unwrap_or_default();
    // Name = span text excluding the <small> class suffix.
    let name = collapse_whitespace(&text_excluding(span, "small"));
    (name, class)
}

// --- any-tree helpers (the page JSON is loosely typed) ---

/// Depth-first search for an object carrying `"name": <name>`.
fn find_named_node<'a>(v: &'a Value, name: &str) -> Option<&'a Value> {
    match v {
        Value::Object(map) => {
            if map.get("name").and_then(Value::as_str) == Some(name) {
                return Some(v);
            }
            map.values().find_map(|child| find_named_node(child, name))
        }
        Value::Array(arr) => arr.iter().find_map(|child| find_named_node(child, name)),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn parse_student_cell_name_and_class() {
        let (name, class) =
            parse_student_cell(r#"<b><span>Baloušek Tomáš<small> (4SA)</small></span> 1,2</b>"#);
        assert_eq!(name, "Baloušek Tomáš");
        assert_eq!(class, "4SA");
    }

    #[test]
    fn parse_student_cell_no_small() {
        let (name, class) = parse_student_cell(r#"<span>Novák Petr</span>"#);
        assert_eq!(name, "Novák Petr");
        assert_eq!(class, "");
    }

    #[test]
    fn parse_course_options_filters_placeholders_and_marks_splits() {
        // class_course node with the "my" view: a placeholder, a section label,
        // a whole-class course, and an indented half-group.
        let node = json!({
            "name": "class_course",
            "data": {"data": [
                {"d": ["my", "-- Moje kurzy --"], "c": [
                    {"d": ["__NULL__", "Vyberte kurz >>"]},
                    {"d": ["label-active", "-- AKTIVNÍ KURZY ---"]},
                    {"d": ["myc-22909-20037", "AUT - 4SA"]},
                    {"d": ["myc-22909-20102", "  AUT 1 - 4SA"]}
                ]}
            ]}
        });
        let (courses, pgroup) = parse_course_options(&node).unwrap();
        assert_eq!(pgroup, "my");
        assert_eq!(courses.len(), 2);
        assert_eq!(courses[0].course_id, "myc-22909-20037");
        assert_eq!(courses[0].name, "AUT - 4SA");
        assert!(!courses[0].split_group);
        assert_eq!(courses[1].name, "AUT 1 - 4SA");
        assert!(
            courses[1].split_group,
            "indented label is a split half-group"
        );
    }

    #[test]
    fn course_options_prefers_my_view_falls_back_to_first() {
        let views = vec![
            json!({"d": ["other", "Other"], "c": [{"d": ["c1", "X"]}]}),
            json!({"d": ["my", "Mine"], "c": [{"d": ["c2", "Y"]}]}),
        ];
        let (opts, pgroup) = course_options_for_view(&views, "my");
        assert_eq!(pgroup, "my");
        assert_eq!(opts.len(), 1);

        let (_, pgroup2) = course_options_for_view(&views, "nonexistent");
        assert_eq!(pgroup2, "other", "falls back to first view");
    }

    #[test]
    fn find_named_node_dfs() {
        let v = json!({"a": {"b": [{"name": "target", "x": 1}]}});
        let node = find_named_node(&v, "target").unwrap();
        assert_eq!(node["x"], json!(1));
        assert!(find_named_node(&v, "missing").is_none());
    }
}

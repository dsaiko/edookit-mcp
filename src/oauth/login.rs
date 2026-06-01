//! Login form rendering. Port of Go's `internal/oauth/login.go` — but the HTML
//! lives in `templates/login.html` and is rendered by askama, whose contextual
//! auto-escaping neutralizes injection in the displayed client_name /
//! redirect_uri (Go relied on `html/template` for the same).

use askama::Template;

/// Passed to the login form. All authorize params survive in hidden inputs so
/// the POST back to `/oauth/authorize` has everything to mint the code.
#[derive(Template)]
#[template(path = "login.html")]
pub struct LoginPage {
    pub title: String,
    pub login_hint: String,
    pub client_id: String,
    pub client_name: String,
    pub redirect_uri: String,
    pub scope: String,
    pub state: String,
    pub code_challenge: String,
    pub code_challenge_method: String,
    pub response_type: String,
    pub error: String,
}

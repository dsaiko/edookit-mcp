package oauth

// loginPageData is what gets passed to the login form template. All authorize
// params survive in hidden inputs so the POST back to /oauth/authorize has
// everything it needs to mint the code and redirect.
type loginPageData struct {
	Title               string
	LoginHint           string // shown above the form if set, e.g. expected username
	ClientID            string
	RedirectURI         string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	ResponseType        string
	Error               string // non-empty on a failed prior submit
}

// loginTemplate is intentionally minimal: one form, two fields. No external
// CSS/JS/font assets, so it works on a phone (ChatGPT iOS opens the consent
// page in an in-app browser) and degrades gracefully without a network.
const loginTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} — Sign in</title>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
         margin: 0; padding: 2em 1em; background: #fafafa; color: #111;
         display: flex; justify-content: center; }
  .card { max-width: 360px; width: 100%; background: white; padding: 1.6em 1.6em 1.4em;
          border-radius: 12px; box-shadow: 0 1px 4px rgba(0,0,0,0.06); }
  h1 { font-size: 1.05rem; margin: 0 0 0.4em; }
  p { color: #555; font-size: 0.85rem; margin: 0 0 1.2em; }
  label { display: block; font-size: 0.78rem; color: #444; margin: 0.9em 0 0.3em; }
  input[type=text], input[type=password] {
    width: 100%; box-sizing: border-box; padding: 0.55em 0.7em;
    border: 1px solid #ccc; border-radius: 8px; font-size: 1rem;
    background: #fff;
  }
  button { margin-top: 1.4em; width: 100%; padding: 0.7em; border: 0;
           border-radius: 8px; background: #1a73e8; color: white;
           font-size: 0.95rem; font-weight: 600; cursor: pointer; }
  button:hover { background: #1664c1; }
  .err { background: #fdecec; color: #8a1a1a; border: 1px solid #f4b6b6;
         padding: 0.5em 0.7em; border-radius: 8px; font-size: 0.85rem;
         margin: 0 0 0.4em; }
  .hint { color: #777; font-size: 0.75rem; margin: 0 0 0.6em; }
</style>
</head>
<body>
<div class="card">
  <h1>{{.Title}}</h1>
  <p>Sign in to authorize this MCP connector.</p>
  {{if .Error}}<div class="err">{{.Error}}</div>{{end}}
  <form method="POST" action="/oauth/authorize" autocomplete="off">
    {{if .LoginHint}}<div class="hint">Expected user: <code>{{.LoginHint}}</code></div>{{end}}
    <label for="u">User</label>
    <input id="u" type="text" name="username" value="{{.LoginHint}}" autocomplete="username">
    <label for="p">Password</label>
    <input id="p" type="password" name="password" autocomplete="current-password" autofocus required>
    <input type="hidden" name="response_type" value="{{.ResponseType}}">
    <input type="hidden" name="client_id" value="{{.ClientID}}">
    <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
    <input type="hidden" name="scope" value="{{.Scope}}">
    <input type="hidden" name="state" value="{{.State}}">
    <input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
    <input type="hidden" name="code_challenge_method" value="{{.CodeChallengeMethod}}">
    <button type="submit">Sign in</button>
  </form>
</div>
</body>
</html>
`

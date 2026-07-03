package main

// pageHTML is the html/template source for the single-book page. The template
// data is a pageView. All fields are auto-escaped by html/template.
const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Your next read{{if .Title}} — {{.Title}}{{end}}</title>
<style>
  :root {
    --bg: #f4f1ea;
    --card: #fffdf8;
    --ink: #2b2620;
    --muted: #6f675b;
    --line: #e4ded1;
    --accent: #7a5c34;
    --accent-ink: #fffdf8;
    --shadow: rgba(43, 38, 32, 0.14);
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #1a1712;
      --card: #24201a;
      --ink: #ece5d8;
      --muted: #a99f8c;
      --line: #3a342b;
      --accent: #c69a63;
      --accent-ink: #1a1712;
      --shadow: rgba(0, 0, 0, 0.5);
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 2rem 1.25rem;
    background: var(--bg);
    color: var(--ink);
    font-family: "Iowan Old Style", "Palatino Linotype", Palatino, Georgia, serif;
    line-height: 1.55;
  }
  .card {
    width: 100%;
    max-width: 640px;
    background: var(--card);
    border: 1px solid var(--line);
    border-radius: 16px;
    box-shadow: 0 18px 40px -18px var(--shadow);
    padding: clamp(1.5rem, 4vw, 2.5rem);
  }
  .eyebrow {
    margin: 0 0 1.25rem;
    font-size: 0.72rem;
    letter-spacing: 0.18em;
    text-transform: uppercase;
    color: var(--muted);
    font-family: ui-sans-serif, system-ui, -apple-system, sans-serif;
  }
  .top { display: flex; gap: 1.5rem; align-items: flex-start; }
  .cover {
    flex: 0 0 auto;
    width: 120px;
    border-radius: 8px;
    box-shadow: 0 8px 20px -8px var(--shadow);
    background: var(--line);
  }
  .head { min-width: 0; }
  h1 {
    margin: 0 0 0.35rem;
    font-size: clamp(1.35rem, 4vw, 1.8rem);
    line-height: 1.2;
  }
  .author { margin: 0; color: var(--muted); font-style: italic; }
  .meta {
    display: flex;
    flex-wrap: wrap;
    gap: 0.4rem 0.75rem;
    margin: 1.25rem 0 0;
    padding: 0;
    list-style: none;
    font-family: ui-sans-serif, system-ui, -apple-system, sans-serif;
    font-size: 0.85rem;
    color: var(--muted);
  }
  .meta li { display: flex; align-items: baseline; gap: 0.3rem; }
  .meta b { color: var(--ink); font-weight: 600; }
  .desc {
    margin: 1.25rem 0 0;
    padding-top: 1.25rem;
    border-top: 1px solid var(--line);
    color: var(--ink);
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.75rem;
    align-items: center;
    margin-top: 1.5rem;
  }
  .btn {
    display: inline-block;
    padding: 0.6rem 1.1rem;
    border-radius: 999px;
    background: var(--accent);
    color: var(--accent-ink);
    text-decoration: none;
    font-family: ui-sans-serif, system-ui, -apple-system, sans-serif;
    font-size: 0.9rem;
    font-weight: 600;
  }
  .btn.secondary {
    background: transparent;
    color: var(--accent);
    border: 1px solid var(--accent);
  }
  @media (max-width: 460px) {
    .top { flex-direction: column; align-items: center; text-align: center; }
    .cover { width: 150px; }
  }
</style>
</head>
<body>
  <main class="card">
    <p class="eyebrow">📚 Your next read</p>
    <div class="top">
      {{if .ImageURL}}<img class="cover" src="{{.ImageURL}}" alt="Cover of {{.Title}}" loading="lazy">{{end}}
      <div class="head">
        <h1>{{.Title}}</h1>
        {{if .Author}}<p class="author">by {{.Author}}</p>{{end}}
        <ul class="meta">
          {{if .Rating}}<li><b>{{.Rating}}</b> avg rating</li>{{end}}
          {{if .Pages}}<li><b>{{.Pages}}</b> pages</li>{{end}}
          {{if .Published}}<li>Published <b>{{.Published}}</b></li>{{end}}
        </ul>
      </div>
    </div>
    {{if .Description}}<p class="desc">{{.Description}}</p>{{end}}
    <div class="actions">
      <a class="btn" href="{{.BookURL}}" target="_blank" rel="noopener">View on Goodreads →</a>
      <a class="btn secondary" href="/">Pick another</a>
    </div>
  </main>
</body>
</html>
`

// The signature of which upload-dialog page is showing, as far as the DOM
// says, or '' when the markup has none of these (doNext then falls back to a
// fixed pause). No args. `vis` is provided by the loader (scripts.go).
() => {
  const d = document.querySelector('ytcp-uploads-dialog'); if (!d) return '';
  const pages = [...d.querySelectorAll('ytcp-uploads-details, ytcp-uploads-video-elements, ytcp-uploads-checks, ytcp-uploads-review')].filter(vis).map(e => e.tagName.toLowerCase());
  const marks = [...d.querySelectorAll('[aria-selected="true"], [aria-current="step"], [active]')].filter(vis).map(e => (e.id || e.tagName.toLowerCase()) + ':' + (e.innerText || '').trim().slice(0, 20));
  return pages.concat(marks).join('|');
}

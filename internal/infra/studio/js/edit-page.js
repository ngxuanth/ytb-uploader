// Reads the video's edit page: title, original file name, and the per-
// resolution processing badges. `ready` is false until the page has loaded.
// No args.
() => {
  const box = document.querySelector('#title-textarea #textbox');
  // Each badge has a hidden "-hover" twin; only the shown ones count.
  const res = [...document.querySelectorAll('#video-resolutions [id^="badge-"]')].filter(b => b.offsetParent !== null && !b.id.endsWith('-hover')).map(b => ({name: b.id.replace('badge-', ''), label: b.getAttribute('aria-label') || ''}));
  return {ready: !!box, title: (box?.textContent || '').trim(), filename: (document.querySelector('#original-filename')?.innerText || '').trim(), res};
}

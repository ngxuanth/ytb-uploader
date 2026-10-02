// Closes the "video is still processing" confirmation Studio shows after
// saving a video that is still being processed; the save is done, the notice
// only covers the page. Returns 'closed' when it clicked, else false.
//
// args: { processing: string, close: string } — regex sources, matched /i.
// `vis` is provided by the loader (scripts.go).
(args) => {
  const processing = new RegExp(args.processing, 'i');
  const close = new RegExp(args.close, 'i');
  const d = [...document.querySelectorAll('tp-yt-paper-dialog, ytcp-dialog, [role=dialog]')].find(d => vis(d) && processing.test(d.innerText || ''));
  if (!d) return false;
  const btn = [...d.querySelectorAll('ytcp-button, button, [role=button]')].find(x => vis(x) && close.test(((x.innerText || '').trim()) || x.getAttribute('aria-label') || ''));
  if (!btn) return false;
  btn.click();
  return 'closed';
}

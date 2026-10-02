// Closes notices Studio pops up over the upload dialog (e.g. "to follow
// YouTube policy, go to 'Use of AI'…"). It only clicks a close button whose
// surrounding block has a known notice text and does not hold the dialog's own
// controls, so it can never close the upload dialog itself. Returns the texts
// of the notices it closed.
//
// args: { notice: string, close: string } — regex sources, matched /i.
// `vis` is provided by the loader (scripts.go).
(args) => {
  const notice = new RegExp(args.notice, 'i');
  const close = new RegExp(args.close, 'i');
  const closed = [];
  for (const b of document.querySelectorAll('button, ytcp-button, tp-yt-paper-icon-button, ytcp-icon-button, [role=button]')) {
    if (!vis(b)) continue;
    const label = ((b.innerText || '').trim() || b.getAttribute('aria-label') || '').trim();
    if (!close.test(label)) continue;
    let a = b.parentElement;
    for (let i = 0; i < 8 && a; i++, a = a.parentElement) {
      if (a.querySelector('#title-textarea, #next-button, #privacy-radios, #done-button')) { a = null; break; }
      if (notice.test(a.innerText || '')) break;
    }
    if (a && notice.test(a.innerText || '')) { closed.push((a.innerText || '').trim().slice(0, 100)); b.click(); }
  }
  return closed;
}

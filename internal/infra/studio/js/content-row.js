// Reads a video's row in the channel's content list: its visibility,
// restrictions and date cells, and whether it is still a draft. Returns
// {found: false} when the row is not on the page.
//
// args: { videoID: string, editDraft: string } — editDraft is a regex source.
(args) => {
  const a = document.querySelector('a[href*="/video/' + args.videoID + '/"]');
  if (!a) return {found: false};
  const row = a.closest('ytcp-video-row') || a.parentElement;
  const g = c => (row.querySelector('.tablecell-' + c)?.innerText || '').trim().replace(/\s+/g, ' ');
  const editDraft = new RegExp(args.editDraft, 'i');
  const draft = [...row.querySelectorAll('ytcp-button, button, a, [role=button]')].some(b => editDraft.test((b.innerText || '') + ' ' + (b.getAttribute('aria-label') || '')));
  return {found: true, visibility: g('visibility'), restrictions: g('restrictions'), date: g('date'), draft};
}

// Finds the video's row in the content list and clicks its "Edit draft"
// button, the way a person continues a draft. Returns 'missing' (no row for
// the video), 'not a draft' (the row has no edit-draft button), or 'ok'.
//
// args: { videoID: string, editDraft: string } — editDraft is a regex source.
(args) => {
  const id = args.videoID;
  const a = document.querySelector('a[href*="/video/' + id + '/"], a[href*="youtu.be/' + id + '"], a[href*="/shorts/' + id + '"]');
  if (!a) return 'missing';
  const row = a.closest('ytcp-video-row, [role=row], tr') || a.parentElement;
  const editDraft = new RegExp(args.editDraft, 'i');
  const b = [...row.querySelectorAll('ytcp-button, button, a, [role=button]')].find(b => editDraft.test((b.innerText || '') + ' ' + (b.getAttribute('aria-label') || '')));
  if (!b) return 'not a draft';
  b.click();
  return 'ok';
}

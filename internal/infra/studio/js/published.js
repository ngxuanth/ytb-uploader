// True once Studio confirmed the save: the share dialog is shown, a
// processing / saved confirmation is shown, or the upload dialog is gone.
//
// args: { confirm: string } — regex source for the confirmation dialog text.
// `vis` is provided by the loader (scripts.go).
(args) => {
  if (vis(document.querySelector('ytcp-video-share-dialog'))) return true;
  const confirm = new RegExp(args.confirm, 'i');
  const dialogs = [...document.querySelectorAll('tp-yt-paper-dialog, ytcp-dialog, [role=dialog]')];
  if (dialogs.some(d => vis(d) && confirm.test(d.innerText || ''))) return true;
  return !vis(document.querySelector('#done-button')) && !vis(document.querySelector('#privacy-radios')) && location.host === 'studio.youtube.com';
}

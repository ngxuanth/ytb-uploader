package studio

import (
	"encoding/json"
	"regexp"
)

// js quotes s as a JavaScript string literal.
func js(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// vis is a JS helper: the element exists and is laid out on screen.
const vis = `const vis = e => !!e && e.offsetParent !== null && getComputedStyle(e).visibility !== 'hidden';`

func iife(body string) string { return "(() => { " + vis + " " + body + " })()" }

var (
	channelInURL = regexp.MustCompile(`/channel/(UC[\w-]{10,})`)
	// A Short's upload dialog links youtube.com/shorts/<id> instead of youtu.be.
	videoInLink = regexp.MustCompile(`(?:youtu\.be/|/video/|/shorts/)([\w-]{11})`)
	percent     = regexp.MustCompile(`(\d{1,3})\s*%`)
)

// interstitialJS closes notices Studio pops up over the upload dialog, such
// as "to follow YouTube policy, go to 'Use of AI'…". It only clicks a close
// button whose surrounding block has a known notice text and does not hold
// the dialog's own controls, so it can never close the upload dialog.
// The notice and button wordings come from the phrase registry (phrases.go).
var interstitialJS = `(() => { ` + vis + `
const notice = ` + jsRE(phAINotice.alts) + `;
const close = ` + jsExact(phCloseButton.alts) + `;
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
return closed; })()`

const dialogOpenJS = `(() => { ` + vis + ` return vis(document.querySelector('ytcp-uploads-dialog #title-textarea #textbox')) || vis(document.querySelector('#privacy-radios')) || vis(document.querySelector('ytcp-uploads-dialog #next-button')); })()`

// stepSigJS tells which page of the upload dialog is showing, as far as the
// DOM says. It is empty when Studio's markup has none of these; doNext then
// falls back to a fixed pause.
const stepSigJS = `(() => { ` + vis + `
const d = document.querySelector('ytcp-uploads-dialog'); if (!d) return '';
const pages = [...d.querySelectorAll('ytcp-uploads-details, ytcp-uploads-video-elements, ytcp-uploads-checks, ytcp-uploads-review')].filter(vis).map(e => e.tagName.toLowerCase());
const marks = [...d.querySelectorAll('[aria-selected="true"], [aria-current="step"], [active]')].filter(vis).map(e => (e.id || e.tagName.toLowerCase()) + ':' + (e.innerText || '').trim().slice(0, 20));
return pages.concat(marks).join('|'); })()`

const privacyJS = `(() => { ` + vis + ` return vis(document.querySelector('#privacy-radios')); })()`

// nextJS presses Next with a DOM click, which skips the extension's
// animated mouse move (about a second per click).
const nextJS = `(() => { const b = document.querySelector('#next-button');
if (!b || b.hasAttribute('disabled') || b.getAttribute('aria-disabled') === 'true') return false;
b.click(); return true; })()`

// publishedJS holds once Studio confirmed the save: the share dialog, the
// "video processing" notice, or the upload dialog gone.
var publishedJS = iife(`if (vis(document.querySelector('ytcp-video-share-dialog'))) return true;
if ([...document.querySelectorAll('tp-yt-paper-dialog, ytcp-dialog, [role=dialog]')].some(d => vis(d) && ` + jsRE(alts(phProcessingDialog, phVideoSaved)) + `.test(d.innerText || ''))) return true;
return !vis(document.querySelector('#done-button')) && !vis(document.querySelector('#privacy-radios')) && location.host === 'studio.youtube.com';`)

// clickJS is a DOM click on the element, centred first: no animated mouse
// move, which makes a real click through the extension take seconds.
func clickJS(sel string) string {
	return iife(`const e = document.querySelector(` + js(sel) + `); if (!e) return false; e.scrollIntoView({block: 'center'}); e.click(); return true;`)
}

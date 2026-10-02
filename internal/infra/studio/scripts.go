package studio

import (
	"embed"
	"encoding/json"
	"regexp"
)

// js quotes s as a JavaScript string literal.
func js(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// vis is a JS helper: the element exists and is laid out on screen. It is the
// prelude put in scope for every embedded script (see eval).
const vis = `const vis = e => !!e && e.offsetParent !== null && getComputedStyle(e).visibility !== 'hidden';`

func iife(body string) string { return "(() => { " + vis + " " + body + " })()" }

// Logic-heavy page scripts live as real .js files so they can be read, linted
// (see scripts_lint_test.go) and edited as JavaScript, not Go strings. Each is
// an arrow function; eval invokes it with the shared prelude in scope and args
// passed as JSON.
//
//go:embed js/*.js
var scriptFS embed.FS

func script(name string) string {
	b, err := scriptFS.ReadFile("js/" + name + ".js")
	if err != nil {
		panic("studio: missing embedded script js/" + name + ".js: " + err.Error())
	}
	return string(b)
}

// eval builds the expression to evaluate on the page: the embedded script
// (name) invoked with args as JSON, with the vis prelude in scope. Pass nil
// args for a script that takes none.
func eval(name string, args any) string {
	call := "()"
	if args != nil {
		a, err := json.Marshal(args)
		if err != nil {
			a = []byte("null")
		}
		call = "(" + string(a) + ")"
	}
	return "(() => { " + vis + " return (" + script(name) + ")" + call + "; })()"
}

var (
	channelInURL = regexp.MustCompile(`/channel/(UC[\w-]{10,})`)
	// A Short's upload dialog links youtube.com/shorts/<id> instead of youtu.be.
	videoInLink = regexp.MustCompile(`(?:youtu\.be/|/video/|/shorts/)([\w-]{11})`)
	percent     = regexp.MustCompile(`(\d{1,3})\s*%`)
)

// interstitialJS closes notices Studio pops up over the upload dialog (see
// js/interstitial.js). The notice and button wordings come from the phrase
// registry (phrases.go).
var interstitialJS = eval("interstitial", map[string]string{
	"notice": reSrc(phAINotice.alts),
	"close":  reSrcExact(phCloseButton.alts),
})

const dialogOpenJS = `(() => { ` + vis + ` return vis(document.querySelector('ytcp-uploads-dialog #title-textarea #textbox')) || vis(document.querySelector('#privacy-radios')) || vis(document.querySelector('ytcp-uploads-dialog #next-button')); })()`

// stepSigJS tells which page of the upload dialog is showing (see
// js/step-signature.js).
var stepSigJS = eval("step-signature", nil)

const privacyJS = `(() => { ` + vis + ` return vis(document.querySelector('#privacy-radios')); })()`

// nextJS presses Next with a DOM click, which skips the extension's
// animated mouse move (about a second per click).
const nextJS = `(() => { const b = document.querySelector('#next-button');
if (!b || b.hasAttribute('disabled') || b.getAttribute('aria-disabled') === 'true') return false;
b.click(); return true; })()`

// publishedJS holds once Studio confirmed the save (see js/published.js): the
// share dialog, a processing / saved confirmation, or the upload dialog gone.
var publishedJS = eval("published", map[string]string{
	"confirm": reSrc(alts(phProcessingDialog, phVideoSaved)),
})

// clickJS is a DOM click on the element, centred first: no animated mouse
// move, which makes a real click through the extension take seconds.
func clickJS(sel string) string {
	return iife(`const e = document.querySelector(` + js(sel) + `); if (!e) return false; e.scrollIntoView({block: 'center'}); e.click(); return true;`)
}

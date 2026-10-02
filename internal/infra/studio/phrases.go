package studio

import (
	"regexp"
	"strings"
)

// Localized UI strings Studio shows on screen, gathered in one place.
//
// Studio is translated, so matching its text means listing the wording of
// every language we support. Each phrase groups the wordings of ONE on-screen
// concept; add a language by adding its wording to the right phrase and
// nothing else moves. A phrase's alts are regex fragments, OR-ed together and
// matched case-insensitively.
//
// Confirmed locales throughout: English (en) and Vietnamese (vi).
type phrase struct {
	name string   // what this matches, for humans reading the registry
	alts []string // regex fragments, OR-ed, matched case-insensitively
}

// phrases, grouped by the UI element they come from.
var (
	// The "to follow YouTube policy…" notice about AI / altered content that
	// pops up over the upload dialog.
	phAINotice = phrase{"ai-notice", []string{
		`sử dụng ai`, `use of ai`, `altered or synthetic`,
		`nội dung (bị )?(thay đổi|chỉnh sửa) hoặc (tổng hợp|tạo)`,
	}}
	// The label of a close / dismiss button. Matched exactly (anchored).
	phCloseButton = phrase{"close-button", []string{
		`đóng`, `close`, `bỏ qua`, `dismiss`, `got it`, `đã hiểu`, `ok`,
	}}
	// The banner shown when the account hit its daily upload limit.
	phUploadLimit = phrase{"upload-limit", []string{
		`daily upload limit`, `upload limit reached`,
		`giới hạn tải (video )?lên hằng ngày`, `đã đạt (đến )?giới hạn`,
	}}
	// The "Edit draft" button in the channel's content list.
	phEditDraft = phrase{"edit-draft", []string{
		`chỉnh sửa bản nháp`, `edit draft`,
	}}
	// The "video is still processing" confirmation shown after a save.
	phProcessingDialog = phrase{"processing-dialog", []string{
		`video processing`, `still processing`, `xử lý video`,
		`vẫn đang (được )?xử lý`,
	}}
	// The "video saved / published" confirmation shown after a save.
	phVideoSaved = phrase{"video-saved", []string{
		`video published`, `video đã được xuất bản`, `video đã được lưu`,
	}}
	// Plain words in the upload-progress label that mean the upload is done
	// and Studio is now processing or checking the video. These are matched
	// as substrings (see labelMeansProcessing), not as a regex.
	phProcessingLabel = phrase{"processing-label", []string{
		`xử lý`, `process`, `kiểm tra`, `check`,
	}}

	// Visibility words in the content-list row and on the edit page.
	phVisUnlisted  = phrase{"visibility-unlisted", []string{`không công khai`, `unlisted`}}
	phVisPrivate   = phrase{"visibility-private", []string{`riêng tư`, `private`}}
	phVisPublic    = phrase{"visibility-public", []string{`công khai`, `public`}}
	phVisScheduled = phrase{"visibility-scheduled", []string{`lên lịch`, `scheduled`}}
	phVisDraft     = phrase{"visibility-draft", []string{`bản nháp`, `draft`}}
	// Resolution-badge labels on the edit page.
	phResDone = phrase{"resolution-done", []string{`đã xử lý xong`, `processed`, `hoàn tất`, `complete`}}
	phResBusy = phrase{"resolution-busy", []string{`đang xử lý`, `processing`}}
)

// alts flattens several phrases' alternatives, keeping their order.
func alts(ps ...phrase) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.alts...)
	}
	return out
}

// jsRE is a case-insensitive JS regex literal /(a|b|c)/i for the alternatives,
// for embedding in a page script.
func jsRE(as []string) string { return "/(" + strings.Join(as, "|") + ")/i" }

// jsExact anchors the match to the whole (trimmed) string: /^(a|b|c)$/i.
func jsExact(as []string) string { return "/^(" + strings.Join(as, "|") + ")$/i" }

// goRE compiles the alternatives to a case-insensitive Go regexp.
func goRE(as []string) *regexp.Regexp {
	return regexp.MustCompile("(?i)(" + strings.Join(as, "|") + ")")
}

// labelMeansProcessing reports whether an upload-progress label says the
// upload is done and Studio is processing or checking the video.
func labelMeansProcessing(label string) bool {
	low := strings.ToLower(label)
	for _, w := range phProcessingLabel.alts {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

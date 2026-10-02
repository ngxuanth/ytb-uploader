package studio

func uploadURL(channel string) string {
	return "https://studio.youtube.com/channel/" + channel + "/videos/upload?d=ud"
}

// StartURL is where the launcher opens the tab before the first Run.
func (t Task) StartURL() string {
	if t.ChannelID != "" {
		return uploadURL(t.ChannelID)
	}
	return "https://studio.youtube.com"
}

func radio(name string) string {
	return `tp-yt-paper-radio-button[name="` + name + `"]`
}

func checked(sel string) string {
	return iife(`return document.querySelector(` + js(sel) + `)?.getAttribute('aria-checked') === 'true';`)
}

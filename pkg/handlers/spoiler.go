package handlers

// spoilerWrap puts a state tag inside an HTML <tg-spoiler> so Telegram
// renders it as a hidden chip the user can tap to reveal — the visible
// chat shows the friendly prompt, the bookkeeping is tucked away. The
// caller MUST send the message with one of the HTML messenger methods.
//
// On the callback / reply side, Telegram delivers q.Message.Text and
// reply_to_message.text with HTML markup stripped, so existing regex-
// based parsing of the tag still works unchanged.
func spoilerWrap(tag string) string {
	return "<tg-spoiler>" + tag + "</tg-spoiler>"
}

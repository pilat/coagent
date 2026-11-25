package sessionprompt

func RenderStopHookFeedback(reason string) string {
	return "A Stop hook kept you from ending your turn:\n\n" + reason +
		"\n\nAddress it, then finish with a complete answer to the original request; your next answer replaces the previous one."
}

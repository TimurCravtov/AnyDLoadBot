package bot

import (
	"context"
	"strings"
	"testing"
)

// In chat the title is the caption (inline messages have none; see the inline tests).
func TestChatVideoHasTitleCaption(t *testing.T) {
	t.Parallel()

	b, tg := newServerBot(t, &slowProvider{})
	b.handleDefault(context.Background(), b.api, chatMessage(1, 5, "slow://abc"))

	send, ok := tg.call("sendVideo")
	if !ok {
		t.Fatalf("no sendVideo; calls: %v", tg.methods())
	}
	if !strings.Contains(send.fields["caption"], "A title") {
		t.Errorf("chat video caption = %q, want the title", send.fields["caption"])
	}
}

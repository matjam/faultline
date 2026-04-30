package telegram

import (
	"strings"
	"testing"
)

func TestToTelegramMarkdown_PassesThroughBasic(t *testing.T) {
	in := "Hello world"
	out, ok := toTelegramMarkdown(in)
	if !ok {
		t.Fatal("expected conversion to succeed")
	}
	if !strings.Contains(out, "Hello world") {
		t.Errorf("output missing original text: %q", out)
	}
}

func TestToTelegramMarkdown_BulletNewlineFix(t *testing.T) {
	// goldmark-tgmd has a bug where paragraph nodes inside list items emit
	// "• \n<text>" instead of "• <text>". Our wrapper strips the extra
	// newline. Verify none of the bullet characters are followed by " \n".
	in := "- item one\n- item two\n- item three\n"
	out, ok := toTelegramMarkdown(in)
	if !ok {
		t.Fatal("expected conversion to succeed")
	}
	for _, bullet := range listBullets {
		if strings.Contains(out, bullet+" \n") {
			t.Errorf("output still contains %q + ' \\n' bug pattern: %q", bullet, out)
		}
	}
}

func TestToTelegramMarkdown_HandlesEmphasis(t *testing.T) {
	in := "This is *bold* and _italic_ text."
	out, ok := toTelegramMarkdown(in)
	if !ok {
		t.Fatal("expected conversion to succeed")
	}
	// We don't assert exact MarkdownV2 output (that's the library's
	// concern); we just assert the conversion produced non-empty output
	// containing the literal words.
	if !strings.Contains(out, "bold") || !strings.Contains(out, "italic") {
		t.Errorf("output missing emphasis content: %q", out)
	}
}

func TestToTelegramMarkdown_EmptyInputFailsGracefully(t *testing.T) {
	out, ok := toTelegramMarkdown("")
	// goldmark with empty input may return empty string; our wrapper
	// reports failure in that case so the caller falls back to plain text.
	if ok && out != "" {
		t.Errorf("empty input produced non-empty success: ok=%v out=%q", ok, out)
	}
}

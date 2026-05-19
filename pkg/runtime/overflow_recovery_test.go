package runtime

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// --- scrubMessage / scrubMessagePart unit tests ---

func TestScrubMessage_TextBelowThreshold_PassesThrough(t *testing.T) {
	t.Parallel()

	msg := chat.Message{
		Role:    chat.MessageRoleUser,
		Content: strings.Repeat("a", 1024),
	}
	out, report := scrubMessage(msg)
	assert.False(t, report.didScrub(), "small text must not be scrubbed")
	assert.Equal(t, msg, out, "small text must pass through byte-identical")
}

func TestScrubMessage_OversizedText_Replaced(t *testing.T) {
	t.Parallel()

	original := strings.Repeat("z", maxScrubbedTextBytes+1)
	msg := chat.Message{Role: chat.MessageRoleUser, Content: original}

	out, report := scrubMessage(msg)
	assert.True(t, report.textReplaced)
	assert.Equal(t, int64(len(original)), report.originalBytes)
	assert.NotEqual(t, original, out.Content, "oversized text must be rewritten")
	assert.Contains(t, out.Content, "too large", "placeholder must signal the cause")
}

func TestScrubMessage_ImageURLPart_BecomesTextPlaceholder(t *testing.T) {
	t.Parallel()

	msg := chat.Message{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "look at this"},
			{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "https://example.com/foo.png"}},
		},
	}

	out, report := scrubMessage(msg)
	require.True(t, report.didScrub())
	assert.Equal(t, 1, report.partsReplaced)
	require.Len(t, out.MultiContent, 2)
	assert.Equal(t, chat.MessagePartTypeText, out.MultiContent[0].Type, "text part untouched")
	assert.Equal(t, "look at this", out.MultiContent[0].Text)
	assert.Equal(t, chat.MessagePartTypeText, out.MultiContent[1].Type, "media replaced by text")
	assert.Contains(t, out.MultiContent[1].Text, "foo.png", "placeholder must include the name when available")
}

func TestScrubMessage_DataURLImage_NameOmitted(t *testing.T) {
	t.Parallel()

	msg := chat.Message{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "data:image/png;base64,AAAA"}},
		},
	}
	out, report := scrubMessage(msg)
	assert.True(t, report.didScrub())
	assert.Equal(t, chat.MessagePartTypeText, out.MultiContent[0].Type)
	// data: URI is not user-meaningful — the placeholder should not
	// leak the base64 payload and should still describe what was
	// removed.
	assert.NotContains(t, out.MultiContent[0].Text, "AAAA")
	assert.Contains(t, out.MultiContent[0].Text, "image attachment")
}

func TestScrubMessage_FilePart_BecomesPlaceholder(t *testing.T) {
	t.Parallel()

	msg := chat.Message{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeFile, File: &chat.MessageFile{Path: "/tmp/big.log"}},
		},
	}
	out, report := scrubMessage(msg)
	assert.True(t, report.didScrub())
	assert.Contains(t, out.MultiContent[0].Text, "big.log")
}

func TestScrubMessage_DocumentPart_IncludesSize(t *testing.T) {
	t.Parallel()

	msg := chat.Message{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
				Name: "report.pdf", MimeType: "application/pdf", Size: 3 * 1024 * 1024,
			}},
		},
	}
	out, report := scrubMessage(msg)
	assert.True(t, report.didScrub())
	assert.Contains(t, out.MultiContent[0].Text, "report.pdf")
	assert.Contains(t, out.MultiContent[0].Text, "MiB", "size should be human-readable")
}

func TestScrubMessage_SmallTextPart_PassesThrough(t *testing.T) {
	t.Parallel()

	msg := chat.Message{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "just text"},
		},
	}
	out, report := scrubMessage(msg)
	assert.False(t, report.didScrub(), "small text in multi-content must not be scrubbed")
	assert.Equal(t, msg, out)
}

// TestScrubMessage_OversizedTextPart_Replaced verifies that an
// oversized text blob inside MultiContent is rewritten just like a
// top-level Content payload. A pure-text overflow can arrive as
// either, and the scrub must catch both shapes.
func TestScrubMessage_OversizedTextPart_Replaced(t *testing.T) {
	t.Parallel()

	original := strings.Repeat("q", maxScrubbedTextBytes+1)
	msg := chat.Message{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "preserved preamble"},
			{Type: chat.MessagePartTypeText, Text: original},
		},
	}
	out, report := scrubMessage(msg)
	assert.True(t, report.didScrub())
	assert.Equal(t, 1, report.partsReplaced)
	require.Len(t, out.MultiContent, 2)
	assert.Equal(t, "preserved preamble", out.MultiContent[0].Text,
		"small text parts must pass through untouched")
	assert.NotEqual(t, original, out.MultiContent[1].Text,
		"oversized text part must be rewritten")
	assert.Contains(t, out.MultiContent[1].Text, "too large")
}

func TestScrubMessage_MultipleMediaParts_AllReplaced(t *testing.T) {
	t.Parallel()

	msg := chat.Message{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "https://a/1.png"}},
			{Type: chat.MessagePartTypeFile, File: &chat.MessageFile{Path: "/tmp/2.log"}},
			{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "3.pdf"}},
		},
	}
	out, report := scrubMessage(msg)
	assert.Equal(t, 3, report.partsReplaced)
	for _, part := range out.MultiContent {
		assert.Equal(t, chat.MessagePartTypeText, part.Type)
	}
}

func TestScrubMessage_TextAndMediaTogether(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("x", maxScrubbedTextBytes+512)
	msg := chat.Message{
		Role:    chat.MessageRoleUser,
		Content: oversized,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "https://a/x.png"}},
		},
	}
	out, report := scrubMessage(msg)
	assert.True(t, report.textReplaced)
	assert.Equal(t, 1, report.partsReplaced)
	assert.NotEqual(t, oversized, out.Content)
	assert.Equal(t, chat.MessagePartTypeText, out.MultiContent[0].Type)
}

// --- recoverFromOversizedTurn integration tests ---

func TestRecoverFromOversizedTurn_NoUserMessage_NoOp(t *testing.T) {
	t.Parallel()

	rt, _ := newTestRuntime(t)
	sess := session.New()
	events := make(chan Event, 4)

	rt.recoverFromOversizedTurn(t.Context(), sess, modelerrors.OverflowKindWire, NewChannelSink(events))
	assert.Empty(t, drainEvents(events), "empty session should produce no events")
}

func TestRecoverFromOversizedTurn_SmallMessage_NoOp(t *testing.T) {
	t.Parallel()

	rt, _ := newTestRuntime(t)
	sess := session.New()
	sess.AddMessage(session.UserMessage("hello"))
	events := make(chan Event, 4)

	rt.recoverFromOversizedTurn(t.Context(), sess, modelerrors.OverflowKindWire, NewChannelSink(events))
	assert.Empty(t, drainEvents(events))
	assert.Equal(t, "hello", sess.GetLastUserMessageContent(), "small message stays verbatim")
}

func TestRecoverFromOversizedTurn_OversizedText_Rewrites(t *testing.T) {
	t.Parallel()

	rt, _ := newTestRuntime(t)
	sess := session.New()
	original := strings.Repeat("Y", maxScrubbedTextBytes+1)
	sess.AddMessage(session.UserMessage(original))
	events := make(chan Event, 4)

	rt.recoverFromOversizedTurn(t.Context(), sess, modelerrors.OverflowKindWire, NewChannelSink(events))
	rewritten := sess.GetLastUserMessageContent()
	assert.NotEqual(t, original, rewritten, "oversized text should have been rewritten")
	assert.Contains(t, rewritten, "too large")

	var sawWarning bool
	for _, ev := range drainEvents(events) {
		if _, ok := ev.(*WarningEvent); ok {
			sawWarning = true
		}
	}
	assert.True(t, sawWarning, "scrub should emit a Warning so the UI can inform the user")
}

func TestRecoverFromOversizedTurn_OnlyLatestUserMessage(t *testing.T) {
	t.Parallel()

	rt, _ := newTestRuntime(t)
	sess := session.New()

	old := strings.Repeat("o", maxScrubbedTextBytes+1)
	sess.AddMessage(session.UserMessage(old))
	// Subsequent assistant + user messages — the scrub must only
	// touch the most recent user turn.
	sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "ok"}})
	sess.AddMessage(session.UserMessage("short"))

	events := make(chan Event, 4)
	rt.recoverFromOversizedTurn(t.Context(), sess, modelerrors.OverflowKindWire, NewChannelSink(events))

	// The latest user message ("short") is small — nothing to scrub.
	assert.Equal(t, "short", sess.GetLastUserMessageContent())
	// The OLDER oversized message must NOT have been touched.
	all := sess.GetAllMessages()
	require.GreaterOrEqual(t, len(all), 1)
	assert.Equal(t, old, all[0].Message.Content,
		"older user messages must not be scrubbed — only the latest is suspect")
	assert.Empty(t, drainEvents(events))
}

// --- handleStreamError integration ---

func TestHandleStreamError_WireOverflowScrubsSession(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New()
	original := strings.Repeat("L", maxScrubbedTextBytes+10)
	sess.AddMessage(session.UserMessage(original))

	events := make(chan Event, 16)
	_, sp := noop.NewTracerProvider().Tracer("t").Start(t.Context(), "x")

	overflow := &modelerrors.ContextOverflowError{
		Underlying: errors.New("HTTP 413: Payload Too Large"),
		Kind:       modelerrors.OverflowKindWire,
	}
	overflowCount := 0

	outcome := rt.handleStreamError(t.Context(), sess, root, overflow, 1000, &overflowCount, sp, NewChannelSink(events))

	assert.Equal(t, streamErrorFatal, outcome)
	assert.NotEqual(t, original, sess.GetLastUserMessageContent(),
		"wire overflow must scrub the offending user turn so future calls in this process don't re-fail")

	// The ErrorEvent for the rejection MUST still be emitted —
	// scrubbing is in addition to the error, not instead of it.
	var sawErrorEvent bool
	for _, ev := range drainEvents(events) {
		if e, ok := ev.(*ErrorEvent); ok {
			sawErrorEvent = true
			assert.Equal(t, ErrorCodeRequestTooLarge, e.Code,
				"wire overflow still surfaces the request-too-large code")
		}
	}
	assert.True(t, sawErrorEvent)
}

// TestHandleStreamError_WireOverflowScrubsEvenWithCompactionDisabled
// pins that the hygiene scrub for wire/media overflow is independent
// of the session-compaction config. Compaction is irrelevant here —
// the scrub rewrites a single message and does not retry, and the
// in-process bug it fixes happens regardless of whether the user
// opted into auto-compaction.
func TestHandleStreamError_WireOverflowScrubsEvenWithCompactionDisabled(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	sess := session.New()
	original := strings.Repeat("W", maxScrubbedTextBytes+10)
	sess.AddMessage(session.UserMessage(original))

	events := make(chan Event, 16)
	_, sp := noop.NewTracerProvider().Tracer("t").Start(t.Context(), "x")

	overflow := &modelerrors.ContextOverflowError{
		Underlying: errors.New("HTTP 413: Payload Too Large"),
		Kind:       modelerrors.OverflowKindWire,
	}
	overflowCount := 0

	outcome := rt.handleStreamError(t.Context(), sess, root, overflow, 1000, &overflowCount, sp, NewChannelSink(events))
	assert.Equal(t, streamErrorFatal, outcome)
	assert.NotEqual(t, original, sess.GetLastUserMessageContent(),
		"scrub must run even when session compaction is disabled")
}

// --- Session.RewriteLatestUserMessage contract ---

func TestSessionRewriteLatestUserMessage_FindsMostRecentUser(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(session.UserMessage("first"))
	sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "reply"}})
	sess.AddMessage(session.UserMessage("second"))

	var seen string
	ok := sess.RewriteLatestUserMessage(func(m chat.Message) (chat.Message, bool) {
		seen = m.Content
		m.Content = "scrubbed"
		return m, true
	})
	assert.True(t, ok)
	assert.Equal(t, "second", seen, "rewrite should target the latest user message")
	assert.Equal(t, "scrubbed", sess.GetLastUserMessageContent())
}

func TestSessionRewriteLatestUserMessage_OptOut_DoesNotMutate(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(session.UserMessage("keep"))

	ok := sess.RewriteLatestUserMessage(func(m chat.Message) (chat.Message, bool) {
		return m, false
	})
	assert.False(t, ok)
	assert.Equal(t, "keep", sess.GetLastUserMessageContent())
}

func TestSessionRewriteLatestUserMessage_NoUserMessages(t *testing.T) {
	t.Parallel()

	sess := session.New()
	ok := sess.RewriteLatestUserMessage(func(m chat.Message) (chat.Message, bool) {
		return m, true
	})
	assert.False(t, ok)
}

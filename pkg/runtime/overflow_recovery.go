package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/session"
)

// maxScrubbedTextBytes is the threshold at and below which a user
// message's plain-text content is preserved verbatim during scrubbing.
// Above the threshold the content is replaced with a placeholder that
// records the original size — keeping it verbatim would re-poison the
// session on the next turn by carrying the offending payload back into
// the provider request.
//
// The value is intentionally well below any major provider's wire-size
// cap. Smaller payloads pass through unchanged.
const maxScrubbedTextBytes = 1 << 20 // 1 MiB

// scrubReport summarises what scrubMessage rewrote, for observability.
// All counters are zero on a no-op scrub.
type scrubReport struct {
	// textReplaced is true when [chat.Message.Content] was over
	// [maxScrubbedTextBytes] and has been replaced.
	textReplaced bool
	// originalBytes is the size of the original plain text, set
	// only when textReplaced is true.
	originalBytes int64
	// partsReplaced counts how many MultiContent parts were
	// rewritten: media parts (image/file/document) are always
	// counted; oversized text parts are also counted when they
	// exceeded [maxScrubbedTextBytes].
	partsReplaced int
}

func (r scrubReport) didScrub() bool {
	return r.textReplaced || r.partsReplaced > 0
}

// scrubMessage returns a copy of msg in which media parts (image_url,
// file, document) are replaced with text placeholders and oversized
// plain-text content is replaced with a size-noting placeholder. The
// returned scrubReport describes what changed; when it reports
// [scrubReport.didScrub] false the message is byte-identical to msg.
//
// scrubMessage is pure: it does not consult the session, the model,
// or any context. Callers decide *when* to apply it (post-failure
// recovery, manual cleanup, etc.); this function only describes
// *how*.
func scrubMessage(msg chat.Message) (chat.Message, scrubReport) {
	var report scrubReport

	out := msg

	// Plain text: replace only when oversized so we don't lose the
	// user's intent for normal-sized messages.
	if len(out.Content) > maxScrubbedTextBytes {
		report.textReplaced = true
		report.originalBytes = int64(len(out.Content))
		out.Content = oversizedTextPlaceholder(int64(len(out.Content)))
	}

	// Multi-content parts: rewrite each media part in place.
	if len(out.MultiContent) > 0 {
		newParts := make([]chat.MessagePart, len(out.MultiContent))
		for i, part := range out.MultiContent {
			rewritten, replaced := scrubMessagePart(part)
			if replaced {
				report.partsReplaced++
			}
			newParts[i] = rewritten
		}
		out.MultiContent = newParts
	}

	return out, report
}

// scrubMessagePart replaces a single attachment part with a text
// placeholder. Returns the rewritten part and whether anything
// changed; small text parts and unrecognised types pass through
// unchanged.
//
// Text parts over [maxScrubbedTextBytes] are themselves rewritten to
// a size-noting placeholder — a single text part inside MultiContent
// can be just as poisoning as oversized [chat.Message.Content].
//
// The placeholder describes what was attached (kind, name, size)
// without preserving any of the content, so the rewritten message
// can never re-trip the provider's media-size limits.
func scrubMessagePart(part chat.MessagePart) (chat.MessagePart, bool) {
	switch part.Type {
	case chat.MessagePartTypeText:
		if int64(len(part.Text)) > maxScrubbedTextBytes {
			return chat.MessagePart{
				Type: chat.MessagePartTypeText,
				Text: oversizedTextPlaceholder(int64(len(part.Text))),
			}, true
		}
		return part, false

	case chat.MessagePartTypeImageURL:
		return chat.MessagePart{
			Type: chat.MessagePartTypeText,
			Text: imagePlaceholder(part),
		}, true

	case chat.MessagePartTypeFile:
		return chat.MessagePart{
			Type: chat.MessagePartTypeText,
			Text: filePlaceholder(part),
		}, true

	case chat.MessagePartTypeDocument:
		return chat.MessagePart{
			Type: chat.MessagePartTypeText,
			Text: documentPlaceholder(part),
		}, true
	}
	// Unknown part type — leave it alone rather than risk dropping
	// data we don't recognise.
	return part, false
}

func oversizedTextPlaceholder(originalBytes int64) string {
	return fmt.Sprintf(
		"[previous message was %s of text — too large for the AI provider; "+
			"content was removed from the session so the conversation can continue]",
		humanByteSize(originalBytes),
	)
}

func imagePlaceholder(part chat.MessagePart) string {
	if part.ImageURL == nil {
		return "[image attachment removed: too large for the AI provider]"
	}
	if name := imageDisplayName(part.ImageURL.URL); name != "" {
		return fmt.Sprintf("[image %q removed: too large for the AI provider]", name)
	}
	return "[image attachment removed: too large for the AI provider]"
}

func filePlaceholder(part chat.MessagePart) string {
	if part.File == nil {
		return "[file attachment removed: too large for the AI provider]"
	}
	name := part.File.Path
	if name != "" {
		name = filepath.Base(name)
	}
	if name == "" {
		name = part.File.FileID
	}
	if name == "" {
		return "[file attachment removed: too large for the AI provider]"
	}
	return fmt.Sprintf("[file %q removed: too large for the AI provider]", name)
}

func documentPlaceholder(part chat.MessagePart) string {
	if part.Document == nil {
		return "[document attachment removed: too large for the AI provider]"
	}
	doc := part.Document
	if doc.Name != "" && doc.Size > 0 {
		return fmt.Sprintf("[document %q (%s) removed: too large for the AI provider]",
			doc.Name, humanByteSize(doc.Size))
	}
	if doc.Name != "" {
		return fmt.Sprintf("[document %q removed: too large for the AI provider]", doc.Name)
	}
	if doc.MimeType != "" {
		return fmt.Sprintf("[%s attachment removed: too large for the AI provider]", doc.MimeType)
	}
	return "[document attachment removed: too large for the AI provider]"
}

// imageDisplayName extracts a short display name from an image URL.
// Returns "" for data: URIs (where the URL itself is the payload and
// not user-meaningful) and falls back to the URL path's basename
// otherwise.
func imageDisplayName(url string) string {
	if url == "" || strings.HasPrefix(url, "data:") {
		return ""
	}
	// Strip query / fragment.
	if i := strings.IndexAny(url, "?#"); i >= 0 {
		url = url[:i]
	}
	base := filepath.Base(url)
	if base == "/" || base == "." {
		return ""
	}
	return base
}

// humanByteSize renders n bytes as a short decimal string with binary
// units (KiB, MiB, GiB). Used for placeholder text only; precision is
// limited to one decimal place since this is informational.
func humanByteSize(n int64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
	}
	return fmt.Sprintf("%d B", n)
}

// recoverFromOversizedTurn rewrites the latest user message in sess so
// that the offending content (oversized text, media attachments) is
// neutralised. This is the runtime's in-memory hygiene step after a
// wire- or media-overflow rejection: without it, the same oversized
// turn re-sends on every subsequent call within this process and the
// conversation cannot continue.
//
// Scope:
//   - In-memory only. The session store row is NOT updated; a
//     docker-agent restart mid-session will reload the original
//     oversized payload from disk. Mirroring the rewrite to the
//     store requires propagating Message.ID from Store.AddMessage
//     back into the in-memory session, which is an independent
//     persistence-layer fix tracked as a separate change.
//   - Only called for [modelerrors.OverflowKindWire] and
//     [modelerrors.OverflowKindMedia]. Token overflow is handled by
//     auto-compaction (a different mechanism).
//   - Mutates only the most recent user message. Earlier turns are
//     left alone — the heuristic is that the latest turn is the one
//     that just tripped the provider; older turns must have been
//     accepted at some point.
func (r *LocalRuntime) recoverFromOversizedTurn(
	ctx context.Context,
	sess *session.Session,
	kind modelerrors.OverflowKind,
	events EventSink,
) {
	var report scrubReport
	rewrote := sess.RewriteLatestUserMessage(func(msg chat.Message) (chat.Message, bool) {
		rewritten, r := scrubMessage(msg)
		if !r.didScrub() {
			return msg, false
		}
		report = r
		return rewritten, true
	})
	if !rewrote {
		// Nothing oversized to scrub (e.g. the offending content was
		// already small, or the session has no user message yet).
		return
	}

	slog.InfoContext(ctx, "Scrubbed oversized user message after overflow",
		"session_id", sess.ID,
		"overflow_kind", string(kind),
		"text_replaced", report.textReplaced,
		"original_text_bytes", report.originalBytes,
		"parts_replaced", report.partsReplaced,
	)
	emitScrubNotice(events, report)
}

// emitScrubNotice surfaces an informational warning so the UI can show
// the user that their message was rewritten in place. Without this the
// recovery is silent and the user sees only "Your message is too
// large" — they wouldn't know that the offending content has been
// removed from the conversation history.
func emitScrubNotice(events EventSink, report scrubReport) {
	if events == nil || !report.didScrub() {
		return
	}
	events.Emit(Warning(
		"Your previous message was too large and has been rewritten in the "+
			"conversation history. Send a smaller message to continue.",
		"",
	))
}

package connector

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

// maxMediaSize caps how large a file the bridge will move in either direction.
// Both homeservers and Nextcloud impose their own limits; this only stops the
// bridge from buffering something enormous in memory before finding out.
const maxMediaSize = 100 << 20

// referenceLookback is how far back to search for a message the bridge just
// created but was not told the ID of. Sharing a file posts the message
// immediately, so it is within the last few even in a busy conversation.
const referenceLookback = 20

var (
	errAttachmentsDisabled = bridgev2.WrapErrorInStatus(errors.New("this Nextcloud server does not allow sharing files into Talk conversations")).
				WithIsCertain(true).WithErrorAsMessage().WithSendNotice(true).
				WithErrorReason(event.MessageStatusUnsupported)
	errMediaTooLarge = bridgev2.WrapErrorInStatus(errors.New("the file is too large for the bridge to move")).
				WithIsCertain(true).WithErrorAsMessage().WithSendNotice(true).
				WithErrorReason(event.MessageStatusUnsupported)
	errRelayCannotSendFiles = bridgev2.WrapErrorInStatus(errors.New("you have no linked Nextcloud account, and the bridge bot cannot upload files")).
				WithIsCertain(true).WithErrorAsMessage().WithSendNotice(true).
				WithErrorReason(event.MessageStatusUnsupported)
)

// mediaDownloader fetches media out of the Matrix media repository. Only the
// bridge's bot implements it in production; tests substitute a stub.
type mediaDownloader interface {
	DownloadMedia(ctx context.Context, uri id.ContentURIString, file *event.EncryptedFileInfo) ([]byte, error)
}

// matrixMedia returns the source for outgoing media, or nil when no bridge is
// attached.
func (c *NCTalkClient) matrixMedia() mediaDownloader {
	if c.downloader != nil {
		return c.downloader
	}
	if c.Main == nil || c.Main.Bridge == nil || c.Main.Bridge.Bot == nil {
		return nil
	}
	return c.Main.Bridge.Bot
}

// isMediaMsgType reports whether a Matrix message carries a file rather than
// only text.
func isMediaMsgType(msgType event.MessageType) bool {
	switch msgType {
	case event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile:
		return true
	}
	return false
}

// msgTypeForMime picks the Matrix message type that renders a file best.
func msgTypeForMime(mimeType string) event.MessageType {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return event.MsgImage
	case strings.HasPrefix(mimeType, "video/"):
		return event.MsgVideo
	case strings.HasPrefix(mimeType, "audio/"):
		return event.MsgAudio
	default:
		return event.MsgFile
	}
}

// convertFileFromTalk turns the file rich object on a Talk message into a
// Matrix media part, downloading the file as the logged-in Nextcloud user and
// reuploading it to the Matrix media repository.
//
// caption is the message text with the file placeholder removed, which Talk
// supports as a caption on the share.
func (c *NCTalkClient) convertFileFromTalk(
	ctx context.Context,
	portal *bridgev2.Portal,
	intent bridgev2.MatrixAPI,
	msg *talkMessage,
	param nctalk.MessageParam,
	caption string,
) (*bridgev2.ConvertedMessagePart, error) {
	// The copy of the rich object delivered over the webhook has its path
	// resolved for nobody in particular — a file the sender keeps in their Talk
	// folder arrives as a bare "note.txt" — so it cannot be fetched with it.
	// Asking for the message as this login resolves the path against their own
	// files, which is the only form WebDAV will accept.
	resolved, err := c.Client.GetMessage(ctx, msg.Token, msg.MessageID)
	if err != nil {
		return nil, fmt.Errorf("resolve file path for message %d: %w", msg.MessageID, err)
	}
	file, ok := resolved.MessageParameters[fileParamKey(resolved.MessageParameters)]
	if !ok || file.Path == "" {
		return nil, fmt.Errorf("message %d has no usable file path", msg.MessageID)
	}

	if size, err := strconv.ParseInt(file.Size, 10, 64); err == nil && size > maxMediaSize {
		return nil, fmt.Errorf("file %q is %d bytes, over the bridge limit", file.Name, size)
	}

	data, err := c.Client.DownloadFile(ctx, file.Path, maxMediaSize)
	if err != nil {
		return nil, fmt.Errorf("download %q: %w", file.Path, err)
	}

	mimeType := file.MimeType
	if mimeType == "" {
		mimeType = mime.TypeByExtension(path.Ext(file.Name))
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	name := file.Name
	if name == "" {
		name = param.Name
	}
	if name == "" {
		name = "file"
	}

	url, encrypted, err := intent.UploadMedia(ctx, portal.MXID, data, name, mimeType)
	if err != nil {
		return nil, fmt.Errorf("upload %q to Matrix: %w", name, err)
	}

	content := &event.MessageEventContent{
		MsgType: msgTypeForMime(mimeType),
		Body:    name,
		URL:     url,
		File:    encrypted,
		Info: &event.FileInfo{
			MimeType: mimeType,
			Size:     len(data),
		},
	}
	// Talk lets a share carry a caption. Matrix renders that as a body separate
	// from the file name, so the file name moves to FileName and the caption
	// becomes the body.
	if caption != "" {
		content.FileName = name
		content.Body = caption
	}
	if w, err := strconv.Atoi(file.Width); err == nil {
		content.Info.Width = w
	}
	if h, err := strconv.Atoi(file.Height); err == nil {
		content.Info.Height = h
	}

	return &bridgev2.ConvertedMessagePart{
		Type:    event.EventMessage,
		Content: content,
	}, nil
}

// fileParamKey returns the key of the first file rich object in a parameter map.
func fileParamKey(params nctalk.MessageParams) string {
	// "file" is the conventional key and the only one Talk uses for a share, but
	// the map is keyed by placeholder name, so fall back to searching by type.
	if p, ok := params["file"]; ok && p.Type == nctalk.ParamTypeFile {
		return "file"
	}
	for key, p := range params {
		if p.Type == nctalk.ParamTypeFile {
			return key
		}
	}
	return ""
}

// sendFileToTalk uploads a Matrix file into the sender's Nextcloud files and
// shares it into the conversation, which is what posts it as a chat message.
func (c *NCTalkClient) sendFileToTalk(ctx context.Context, token string, msg *bridgev2.MatrixMessage) (*nctalk.Message, error) {
	caps := c.capabilities()
	if !caps.AttachmentsAllowed() {
		return nil, errAttachmentsDisabled
	}

	source := c.matrixMedia()
	if source == nil {
		return nil, fmt.Errorf("no Matrix media source is available to download the file from")
	}

	content := msg.Content
	data, err := source.DownloadMedia(ctx, content.URL, content.File)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", bridgev2.ErrMediaDownloadFailed, err)
	}
	if len(data) > maxMediaSize {
		return nil, errMediaTooLarge
	}

	folder := caps.AttachmentFolder()
	if err := c.Client.EnsureFolder(ctx, folder); err != nil {
		return nil, fmt.Errorf("prepare the Talk attachment folder: %w", err)
	}

	// WebDAV PUT overwrites silently, so a name already in use has to be found
	// and stepped around rather than discovered afterwards.
	remotePath, err := c.Client.FreeFilePath(ctx, folder, outgoingFileName(content))
	if err != nil {
		return nil, err
	}

	mimeType := content.GetInfo().MimeType
	if err := c.Client.UploadFile(ctx, remotePath, data, mimeType); err != nil {
		return nil, fmt.Errorf("%w: %w", bridgev2.ErrMediaReuploadFailed, err)
	}

	ref := referenceID(msg.Event.ID)
	share := nctalk.ShareFileRequest{
		Path:        remotePath,
		Token:       token,
		ReferenceID: ref,
		Caption:     outgoingCaption(content),
	}
	if err := c.Client.ShareFileToConversation(ctx, share); err != nil {
		return nil, c.wrapSendError(ctx, err)
	}

	// Sharing creates the chat message but reports only the share, so the ID
	// Talk assigned has to be recovered from the reference it stored. Without
	// it the message would echo back over the webhook as somebody else's.
	sent, err := c.Client.FindMessageByReference(ctx, token, ref, referenceLookback)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).
			Str("token", token).
			Str("reference_id", ref).
			Msg("Shared a file but could not find the message Talk created for it; it may be bridged back as a duplicate")
		return nil, nil
	}
	return sent, nil
}

// outgoingFileName picks the name to store a Matrix file under.
func outgoingFileName(content *event.MessageEventContent) string {
	if content.FileName != "" {
		return content.FileName
	}
	if content.Body != "" {
		return content.Body
	}
	return "file"
}

// outgoingCaption returns the caption on a Matrix media message, which is the
// body when it differs from the file name.
func outgoingCaption(content *event.MessageEventContent) string {
	if content.FileName == "" || content.FileName == content.Body {
		return ""
	}
	return content.Body
}

// handleMatrixMedia posts a Matrix file message into Talk.
func (c *NCTalkClient) handleMatrixMedia(ctx context.Context, host, token string, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	// The bot endpoint can only post text, and uploading as the bridge's own
	// account would put a stranger's file in a real person's Nextcloud.
	if msg.OrigSender != nil {
		return nil, errRelayCannotSendFiles
	}

	sent, err := c.sendFileToTalk(ctx, token, msg)
	if err != nil {
		return nil, err
	}

	dbMsg := &database.Message{
		SenderID: makeUserID(host, nctalk.ActorUsers, c.meta().Username),
		Metadata: &MessageMetadata{ReferenceID: referenceID(msg.Event.ID)},
	}
	if sent == nil {
		// The file is in Talk; only its ID is unknown. Recording it under the
		// reference keeps the Matrix event mapped to something.
		dbMsg.ID = makeRelayedMessageID(host, token, referenceID(msg.Event.ID))
		return &bridgev2.MatrixMessageResponse{DB: dbMsg}, nil
	}

	dbMsg.ID = makeMessageID(host, token, sent.ID)
	// Talk's own timestamp, not the Matrix one, because Talk measures its
	// edit and delete windows from it.
	dbMsg.Timestamp = time.Unix(sent.Timestamp, 0)
	return &bridgev2.MatrixMessageResponse{DB: dbMsg, StreamOrder: sent.ID}, nil
}

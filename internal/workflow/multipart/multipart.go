// Package multipart builds a JSON input payload from a streaming
// multipart/form-data body: text parts become arrays of strings, file parts
// stream part-by-part into object storage and become arrays of file
// references. Every field lands in context as an array, with no exceptions.
//
// A part carrying a filename is a file; a part without one is a string
// field, so service-to-service callers work with no form. Files are
// validated twice: hard size/MIME checks fail fast while streaming, and the
// node's validation script then applies per-field rules against the enriched
// references (size, mime_type, array length).
package multipart

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	stdmultipart "mime/multipart"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/objectstore"
	"github.com/beelinewf/wf-engine/pkg/ids"
)

// sniffLen is the MIME sniff window: http.DetectContentType considers at
// most the first 512 bytes.
const sniffLen = 512

// Limits bounds one streaming multipart delivery. Breaches fail the
// delivery; the caller deletes the uploaded keys the Result carries.
type Limits struct {
	// MaxFiles caps the file-part count of one delivery.
	MaxFiles int
	// MaxTotalBytes caps the combined file and text bytes of one delivery.
	MaxTotalBytes int64
	// MaxFileBytes caps one file when the input node sets no
	// max_file_bytes of its own.
	MaxFileBytes int64
	// MaxTextBytes caps one text part.
	MaxTextBytes int64
}

// DefaultLimits returns the engine streaming defaults. Configuration
// overrides them; a non-positive field falls back to its default.
func DefaultLimits() Limits {
	return Limits{
		MaxFiles:      20,
		MaxTotalBytes: 128 << 20,
		MaxFileBytes:  32 << 20,
		MaxTextBytes:  1 << 20,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxFiles <= 0 {
		l.MaxFiles = d.MaxFiles
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = d.MaxTotalBytes
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxTextBytes <= 0 {
		l.MaxTextBytes = d.MaxTextBytes
	}
	return l
}

// FileRef is the context shape of one uploaded file: names, counted bytes,
// and the sniffed MIME type. The validation script reads size, mime_type,
// and array length from these.
type FileRef struct {
	OriginalFileName string `json:"original_file_name"`
	FileName         string `json:"file_name"`
	Size             int64  `json:"size"`
	MIMEType         string `json:"mime_type"`
}

// Result is a built delivery: the synthesized JSON payload plus the
// uploaded objects. Keys is populated even on failure, so the caller
// deletes every committed object; Payload is set only on success. Build
// always returns a non-nil Result.
type Result struct {
	Payload []byte
	Bucket  string
	Keys    []string
}

// ReaderFactory opens the streaming multipart reader of one delivery. The
// handler passes the request body through without reading it; the service
// pulls parts only after the delivery is authorized.
type ReaderFactory func() (*stdmultipart.Reader, error)

// Build consumes the multipart body part by part — never buffering a file
// in memory or on disk — and synthesizes the JSON payload the existing
// validation/merge tail runs on. Text fields become arrays of strings,
// file fields arrays of FileRef, both in part order.
func Build(ctx context.Context, mr *stdmultipart.Reader, node *model.NodeContent, store objectstore.Store, limits Limits) (*Result, error) {
	limits = limits.withDefaults()
	res := &Result{}
	if node == nil || node.Storage == nil || store == nil {
		return res, errors.New("multipart: node storage and object store are required")
	}
	res.Bucket = node.Storage.Bucket
	fields := map[string]*fieldAcc{}
	var total int64
	var files int
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, fmt.Errorf("%w: cannot read multipart body: %v", model.ErrBadRequest, err)
		}
		name, err := normalizeFieldName(part.FormName())
		if err != nil {
			return res, err
		}
		if !isFilePart(part) {
			if err := appendText(part, fields, name, limits, &total); err != nil {
				return res, err
			}
			continue
		}
		filename := part.FileName()
		if filename == "" {
			// An untouched browser file input: skipped, contributing no
			// array entry and no field type.
			continue
		}
		acc, err := fieldFor(fields, name, true)
		if err != nil {
			return res, err
		}
		files++
		if files > limits.MaxFiles {
			return res, fmt.Errorf("%w: at most %d files per delivery", model.ErrPayloadTooLarge, limits.MaxFiles)
		}
		ref, key, size, err := uploadFile(ctx, part, filename, node, store, limits)
		if err != nil {
			if key != "" {
				// The failed upload may have committed a partial
				// object; the caller deletes it best-effort.
				res.Keys = append(res.Keys, key)
			}
			return res, err
		}
		total += size
		if total > limits.MaxTotalBytes {
			res.Keys = append(res.Keys, key)
			return res, fmt.Errorf("%w: multipart body exceeds %d bytes in total", model.ErrPayloadTooLarge, limits.MaxTotalBytes)
		}
		res.Keys = append(res.Keys, key)
		acc.files = append(acc.files, ref)
	}
	payload := make(map[string]any, len(fields))
	for name, acc := range fields {
		if acc.isFile {
			payload[name] = acc.files
		} else {
			payload[name] = acc.texts
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return res, fmt.Errorf("multipart: encode payload: %w", err)
	}
	res.Payload = raw
	return res, nil
}

// fieldAcc collects one normalized field's entries in part order.
type fieldAcc struct {
	isFile bool
	texts  []string
	files  []FileRef
}

// fieldFor resolves the accumulator for a part, creating it on first use.
// A key receiving both text and file parts is a 400: the two shapes cannot
// share one array.
func fieldFor(fields map[string]*fieldAcc, name string, isFile bool) (*fieldAcc, error) {
	acc := fields[name]
	if acc == nil {
		acc = &fieldAcc{isFile: isFile}
		fields[name] = acc
		return acc, nil
	}
	if acc.isFile != isFile {
		return nil, fmt.Errorf("%w: field %q mixes text and file parts", model.ErrBadRequest, name)
	}
	return acc, nil
}

// normalizeFieldName strips one trailing "[]" so "images" and "images[]"
// merge in part order. Any other bracket form is a 400.
func normalizeFieldName(name string) (string, error) {
	name = strings.TrimSuffix(name, "[]")
	if name == "" {
		return "", fmt.Errorf("%w: multipart part has no field name", model.ErrBadRequest)
	}
	if strings.ContainsAny(name, "[]") {
		return "", fmt.Errorf("%w: field %q: only a single trailing [] is allowed", model.ErrBadRequest, name)
	}
	return name, nil
}

// isFilePart reports whether the part carries a filename parameter —
// present even when empty for an untouched browser file input — which is
// what distinguishes a file part from a string field. Part.FileName alone
// cannot tell an empty filename from a missing one.
func isFilePart(part *stdmultipart.Part) bool {
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil {
		return false
	}
	if _, ok := params["filename"]; ok {
		return true
	}
	_, ok := params["filename*"]
	return ok
}

// appendText reads one text part under the per-part cap and appends it.
func appendText(part *stdmultipart.Part, fields map[string]*fieldAcc, name string, limits Limits, total *int64) error {
	acc, err := fieldFor(fields, name, false)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(part, limits.MaxTextBytes+1))
	if err != nil {
		return fmt.Errorf("%w: cannot read multipart field %q: %v", model.ErrBadRequest, name, err)
	}
	if int64(len(data)) > limits.MaxTextBytes {
		return fmt.Errorf("%w: field %q exceeds %d bytes", model.ErrPayloadTooLarge, name, limits.MaxTextBytes)
	}
	*total += int64(len(data))
	if *total > limits.MaxTotalBytes {
		return fmt.Errorf("%w: multipart body exceeds %d bytes in total", model.ErrPayloadTooLarge, limits.MaxTotalBytes)
	}
	acc.texts = append(acc.texts, string(data))
	return nil
}

// errFileTooLarge aborts a streaming upload past the per-file cap.
var errFileTooLarge = errors.New("multipart: file exceeds its byte cap")

// uploadFile sniffs one file part's MIME type, checks it against the node's
// allowlist before the upload starts, then streams peek-plus-remainder into
// the object store under prefix + uuid + sanitized extension. It returns the
// file reference, the object key, and the counted bytes.
func uploadFile(ctx context.Context, part *stdmultipart.Part, filename string, node *model.NodeContent, store objectstore.Store, limits Limits) (FileRef, string, int64, error) {
	var ref FileRef
	peek, err := readPeek(part)
	if err != nil {
		return ref, "", 0, fmt.Errorf("%w: cannot read multipart file %q: %v", model.ErrBadRequest, filename, err)
	}
	effective := effectiveMIMEType(peek, part.Header.Get("Content-Type"))
	if !mimeAllowed(node.Storage.AllowedMIMETypes, effective) {
		return ref, "", 0, fmt.Errorf("%w: file %q has mime type %q, which this input does not accept", model.ErrInvalid, filename, effective)
	}
	cap := node.Storage.MaxFileBytes
	if cap <= 0 {
		cap = limits.MaxFileBytes
	}
	id, err := ids.NewString()
	if err != nil {
		return ref, "", 0, fmt.Errorf("multipart: mint object name: %w", err)
	}
	fileName := id
	if ext := sanitizeExtension(filename); ext != "" {
		fileName += "." + ext
	}
	key := node.Storage.Prefix + fileName
	body := &cappedReader{r: io.MultiReader(bytes.NewReader(peek), part), remaining: cap + 1}
	putErr := store.Put(ctx, node.Storage.Bucket, key, body, -1, effective, map[string]string{
		"original-file-name": filename,
		"mime-type":          effective,
	})
	if putErr != nil {
		if errors.Is(putErr, errFileTooLarge) || errors.Is(body.err, errFileTooLarge) {
			return ref, key, 0, fmt.Errorf("%w: file %q exceeds %d bytes", model.ErrPayloadTooLarge, filename, cap)
		}
		return ref, key, 0, putErr
	}
	if errors.Is(body.err, errFileTooLarge) {
		// A Store that swallowed the stream error and reported success:
		// the cap still decides.
		return ref, key, 0, fmt.Errorf("%w: file %q exceeds %d bytes", model.ErrPayloadTooLarge, filename, cap)
	}
	return FileRef{
		OriginalFileName: filename,
		FileName:         fileName,
		Size:             body.read,
		MIMEType:         effective,
	}, key, body.read, nil
}

// readPeek reads up to the sniff window from the part.
func readPeek(part *stdmultipart.Part) ([]byte, error) {
	buf := make([]byte, sniffLen)
	n, err := io.ReadFull(part, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return buf[:n], nil
	}
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// effectiveMIMEType is the sniffed type of the file's first bytes, falling
// back to the part's declared Content-Type only when sniffing yields
// application/octet-stream. The declared header alone never satisfies an
// allowlist entry with a specific type.
func effectiveMIMEType(peek []byte, declared string) string {
	sniffed := mediaTypeOf(http.DetectContentType(peek))
	if sniffed != "" && sniffed != "application/octet-stream" {
		return sniffed
	}
	if declared := mediaTypeOf(declared); declared != "" {
		return declared
	}
	return "application/octet-stream"
}

// mediaTypeOf normalizes a Content-Type value to its bare lowercase media
// type, dropping parameters. Unparsable values read as absent.
func mediaTypeOf(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(v)
	if err != nil {
		return ""
	}
	return strings.ToLower(mediaType)
}

// mimeAllowed matches the effective type against the node's allowlist: a
// "type/*" entry matches any subtype, a "type/subtype" entry matches
// exactly. An empty allowlist accepts everything.
func mimeAllowed(allow []string, effective string) bool {
	if len(allow) == 0 {
		return true
	}
	mediaType, subtype, ok := strings.Cut(effective, "/")
	if !ok {
		return false
	}
	for _, entry := range allow {
		wantType, wantSub, ok := strings.Cut(entry, "/")
		if !ok {
			continue
		}
		if wantType == mediaType && (wantSub == "*" || wantSub == subtype) {
			return true
		}
	}
	return false
}

// sanitizeExtension is the lowercased suffix of the original filename past
// its last dot, kept to [a-z0-9]. No dot means a bare UUID key.
func sanitizeExtension(filename string) string {
	i := strings.LastIndexByte(filename, '.')
	if i < 0 || i+1 >= len(filename) {
		return ""
	}
	var b strings.Builder
	for _, r := range strings.ToLower(filename[i+1:]) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cappedReader aborts the stream past the per-file cap: the first read past
// it fails with errFileTooLarge, which fails the Put so a partial object is
// never committed. It also counts the streamed bytes for the file reference.
type cappedReader struct {
	r         io.Reader
	remaining int64
	read      int64
	err       error
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.remaining <= 0 {
		c.err = errFileTooLarge
		return 0, c.err
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	c.read += int64(n)
	if c.remaining == 0 && err == nil {
		// At cap+1 bytes the file is over: fail now rather than on the
		// next read, so a Put that stops early cannot commit the excess.
		c.err = errFileTooLarge
		return n, c.err
	}
	return n, err
}

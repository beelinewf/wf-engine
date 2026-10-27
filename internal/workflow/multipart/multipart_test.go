package multipart_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	stdmultipart "mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/multipart"
	"github.com/beelinewf/wf-engine/pkg/ids"
)

var testLimits = model.NodeLimits{
	DefaultTimeout:   30 * time.Second,
	MaxTimeout:       5 * time.Minute,
	ConditionTimeout: 5 * time.Second,
}

func testNode(t *testing.T, raw string) *model.NodeContent {
	t.Helper()
	nc, err := model.ParseNodeContent([]byte(raw), testLimits)
	if err != nil {
		t.Fatalf("ParseNodeContent(%s) error = %v", raw, err)
	}
	return nc
}

// fakeObject is one committed object.
type fakeObject struct {
	data        []byte
	contentType string
	metadata    map[string]string
}

type putCall struct {
	bucket      string
	key         string
	contentType string
	metadata    map[string]string
	size        int64
}

// fakeStore records uploads in part order. A Put whose reader fails commits
// nothing, mirroring the aborted S3 upload; every attempt is still recorded
// so tests can prove what the builder tried.
type fakeStore struct {
	mu      sync.Mutex
	objects map[string]fakeObject
	puts    []putCall
	deletes []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string]fakeObject{}}
}

func (f *fakeStore) Put(_ context.Context, bucket, key string, r io.Reader, size int64, contentType string, userMetadata map[string]string) error {
	data, err := io.ReadAll(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, putCall{bucket: bucket, key: key, contentType: contentType, metadata: userMetadata, size: size})
	if err != nil {
		return err
	}
	f.objects[bucket+"/"+key] = fakeObject{data: data, contentType: contentType, metadata: userMetadata}
	return nil
}

func (f *fakeStore) Delete(_ context.Context, bucket, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, bucket+"/"+key)
	delete(f.objects, bucket+"/"+key)
	return nil
}

func (f *fakeStore) object(bucket, key string) (fakeObject, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[bucket+"/"+key]
	return o, ok
}

func (f *fakeStore) objectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.objects)
}

func (f *fakeStore) putCalls() []putCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]putCall(nil), f.puts...)
}

// partSpec describes one multipart part. A nil header builds a text part;
// filename with empty name builds an untouched file input.
type partSpec struct {
	field       string
	filename    string
	isFile      bool
	contentType string
	data        []byte
}

func buildBody(t *testing.T, parts []partSpec) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := stdmultipart.NewWriter(&buf)
	for _, p := range parts {
		if !p.isFile {
			fw, err := w.CreateFormField(p.field)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fw.Write(p.data); err != nil {
				t.Fatal(err)
			}
			continue
		}
		h := textproto.MIMEHeader{}
		disp := fmt.Sprintf(`form-data; name=%q; filename=%q`, p.field, p.filename)
		h.Set("Content-Disposition", disp)
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		fw, err := w.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(p.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

func testReader(t *testing.T, body []byte, contentType string) *stdmultipart.Reader {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("parse content type: %v", err)
	}
	return stdmultipart.NewReader(bytes.NewReader(body), params["boundary"])
}

func buildParts(t *testing.T, node *model.NodeContent, store *fakeStore, limits multipart.Limits, parts []partSpec) (*multipart.Result, error) {
	t.Helper()
	body, ct := buildBody(t, parts)
	return multipart.Build(context.Background(), testReader(t, body, ct), node, store, limits)
}

func decodePayload(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("payload is not valid JSON: %v (%s)", err, raw)
	}
	return payload
}

func asArray(t *testing.T, payload map[string]any, field string) []any {
	t.Helper()
	v, ok := payload[field]
	if !ok {
		t.Fatalf("payload has no field %q (%v)", field, payload)
	}
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("field %q = %T, want an array", field, v)
	}
	return arr
}

var multipartNode = `{"type":"input","channel":"http","accept":["application/json","multipart/form-data"],` +
	`"output_property":"submission","storage":{"bucket":"uploads","prefix":"submissions/"}}`

// Every text field lands as an array of strings in part order, and a
// trailing [] merges with the bare name.
func TestBuildTextFieldsAreUniformArrays(t *testing.T) {
	node := testNode(t, multipartNode)
	store := newFakeStore()
	res, err := buildParts(t, node, store, multipart.DefaultLimits(), []partSpec{
		{field: "name", data: []byte("Ada")},
		{field: "tags", data: []byte("a")},
		{field: "tags[]", data: []byte("b")},
		{field: "tags", data: []byte("c")},
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	payload := decodePayload(t, res.Payload)
	name := asArray(t, payload, "name")
	if len(name) != 1 || name[0] != "Ada" {
		t.Errorf("name = %v, want [Ada]", name)
	}
	tags := asArray(t, payload, "tags")
	if len(tags) != 3 || tags[0] != "a" || tags[1] != "b" || tags[2] != "c" {
		t.Errorf("tags = %v, want [a b c] in part order", tags)
	}
	if store.objectCount() != 0 || len(res.Keys) != 0 {
		t.Errorf("text-only build uploaded %d objects, want none", store.objectCount())
	}
}

// Every file field lands as a one-element-or-more array of the four-key
// reference, and images[] merges with images in part order.
func TestBuildFileFieldsAreUniformArrays(t *testing.T) {
	node := testNode(t, multipartNode)
	store := newFakeStore()
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte("j"), 100)...)
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte("p"), 50)...)
	res, err := buildParts(t, node, store, multipart.DefaultLimits(), []partSpec{
		{field: "avatar", filename: "me.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
		{field: "images", filename: "a.png", isFile: true, contentType: "image/png", data: png},
		{field: "images[]", filename: "b.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	payload := decodePayload(t, res.Payload)
	avatar := asArray(t, payload, "avatar")
	if len(avatar) != 1 {
		t.Fatalf("avatar has %d entries, want a one-element array", len(avatar))
	}
	images := asArray(t, payload, "images")
	if len(images) != 2 {
		t.Fatalf("images has %d entries, want 2 merged in part order", len(images))
	}
	first, ok := images[0].(map[string]any)
	if !ok {
		t.Fatalf("images[0] = %T, want a file reference object", images[0])
	}
	if first["original_file_name"] != "a.png" {
		t.Errorf("original_file_name = %v, want a.png", first["original_file_name"])
	}
	second, _ := images[1].(map[string]any)
	if second["original_file_name"] != "b.jpg" {
		t.Errorf("images[1].original_file_name = %v, want b.jpg (part order)", second["original_file_name"])
	}
	for i, e := range images {
		ref, _ := e.(map[string]any)
		for _, k := range []string{"original_file_name", "file_name", "size", "mime_type"} {
			if _, ok := ref[k]; !ok {
				t.Errorf("images[%d] has no %q (%v)", i, k, ref)
			}
		}
		if len(ref) != 4 {
			t.Errorf("images[%d] has %d keys, want exactly the four reference keys", i, len(ref))
		}
	}
}

// The mixed body from the plan: one text part plus three file parts, with
// prefixed keys, uuid file names, sniffed MIME metadata, and counted sizes.
func TestBuildMixedBodyShape(t *testing.T) {
	node := testNode(t, multipartNode)
	store := newFakeStore()
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte("j"), 1000)...)
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte("p"), 2000)...)
	res, err := buildParts(t, node, store, multipart.DefaultLimits(), []partSpec{
		{field: "name", data: []byte("Ada")},
		{field: "images[]", filename: "a.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
		{field: "images[]", filename: "b.png", isFile: true, contentType: "image/png", data: png},
		{field: "images[]", filename: "C.JPG", isFile: true, contentType: "image/jpeg", data: jpeg},
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if res.Bucket != "uploads" {
		t.Errorf("bucket = %q, want uploads", res.Bucket)
	}
	if len(res.Keys) != 3 {
		t.Fatalf("keys = %v, want 3 uploaded objects", res.Keys)
	}
	payload := decodePayload(t, res.Payload)
	if got := asArray(t, payload, "name"); len(got) != 1 || got[0] != "Ada" {
		t.Errorf("name = %v, want [Ada]", got)
	}
	images := asArray(t, payload, "images")
	if len(images) != 3 {
		t.Fatalf("images has %d entries, want 3", len(images))
	}
	puts := store.putCalls()
	if len(puts) != 3 {
		t.Fatalf("store saw %d puts, want 3 in part order", len(puts))
	}
	wantExt := []string{".jpg", ".png", ".jpg"}
	for i, e := range images {
		ref, _ := e.(map[string]any)
		fileName, _ := ref["file_name"].(string)
		stem := strings.TrimSuffix(fileName, wantExt[i])
		if !strings.HasSuffix(fileName, wantExt[i]) || !ids.Valid(stem) {
			t.Errorf("file_name = %q, want <uuidv7>%s", fileName, wantExt[i])
		}
		if res.Keys[i] != "submissions/"+fileName {
			t.Errorf("key = %q, want submissions/%s", res.Keys[i], fileName)
		}
		// The stored bytes are exactly the uploaded part: streaming never
		// alters content, and the counted size matches.
		obj, ok := store.object("uploads", res.Keys[i])
		if !ok {
			t.Fatalf("bucket holds no object for key %q", res.Keys[i])
		}
		wantData := jpeg
		if i == 1 {
			wantData = png
		}
		if !bytes.Equal(obj.data, wantData) {
			t.Errorf("object %d holds %d bytes, want the %d uploaded bytes", i, len(obj.data), len(wantData))
		}
		if ref["size"] != float64(len(wantData)) {
			t.Errorf("size = %v, want %d counted bytes", ref["size"], len(wantData))
		}
		wantMIME := "image/jpeg"
		if i == 1 {
			wantMIME = "image/png"
		}
		if ref["mime_type"] != wantMIME {
			t.Errorf("mime_type = %v, want the sniffed %s", ref["mime_type"], wantMIME)
		}
		if obj.contentType != wantMIME {
			t.Errorf("object content type = %q, want %s", obj.contentType, wantMIME)
		}
		if obj.metadata["mime-type"] != wantMIME {
			t.Errorf("object mime-type metadata = %q, want %s", obj.metadata["mime-type"], wantMIME)
		}
		// Uploads happen in part order: the third put carries the .JPG.
		if puts[i].key != res.Keys[i] {
			t.Errorf("put %d key = %q, want part order %q", i, puts[i].key, res.Keys[i])
		}
	}
	if got := images[0].(map[string]any)["original_file_name"]; got != "a.jpg" {
		t.Errorf("images[0].original_file_name = %v, want a.jpg", got)
	}
	if got := puts[0].metadata["original-file-name"]; got != "a.jpg" {
		t.Errorf("original-file-name metadata = %q, want a.jpg", got)
	}
}

// An untouched browser file input (empty filename) is skipped: it
// contributes no entry, and a field left with no entries is omitted.
func TestBuildSkipsEmptyFilename(t *testing.T) {
	node := testNode(t, multipartNode)
	store := newFakeStore()
	res, err := buildParts(t, node, store, multipart.DefaultLimits(), []partSpec{
		{field: "name", data: []byte("Ada")},
		{field: "images", filename: "", isFile: true, contentType: "application/octet-stream", data: nil},
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	payload := decodePayload(t, res.Payload)
	if _, ok := payload["images"]; ok {
		t.Errorf("payload carries images = %v, want the empty-filename field omitted", payload["images"])
	}
	if got := asArray(t, payload, "name"); len(got) != 1 {
		t.Errorf("name = %v, want the surviving field", got)
	}
	if store.objectCount() != 0 {
		t.Errorf("store holds %d objects, want none uploaded", store.objectCount())
	}
}

// One normalized key cannot receive both text and file parts.
func TestBuildRejectsMixedField(t *testing.T) {
	node := testNode(t, multipartNode)
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'j'}
	for name, parts := range map[string][]partSpec{
		"text then file": {
			{field: "mix", data: []byte("x")},
			{field: "mix", filename: "a.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
		},
		"file then text": {
			{field: "mix", filename: "a.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
			{field: "mix", data: []byte("x")},
		},
		"bracket merge": {
			{field: "mix[]", data: []byte("x")},
			{field: "mix", filename: "a.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore()
			res, err := buildParts(t, node, store, multipart.DefaultLimits(), parts)
			if err == nil {
				t.Fatalf("Build() error = nil, want the mixed field rejected")
			}
			if !errors.Is(err, model.ErrBadRequest) {
				t.Errorf("error = %v, want ErrBadRequest (400)", err)
			}
			if res == nil {
				t.Fatal("result is nil, want the keys-so-far for cleanup")
			}
		})
	}
	// The file uploaded before the offending text part is reported for
	// cleanup, so the caller deletes it.
	store := newFakeStore()
	res, err := buildParts(t, node, store, multipart.DefaultLimits(), []partSpec{
		{field: "mix", filename: "a.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
		{field: "mix", data: []byte("x")},
	})
	if err == nil {
		t.Fatal("Build() error = nil, want the mixed field rejected")
	}
	if len(res.Keys) != 1 {
		t.Errorf("keys = %v, want the one uploaded object for cleanup", res.Keys)
	}
}

// Only a single trailing [] is structural; any other bracket form is a 400.
func TestBuildRejectsBadBrackets(t *testing.T) {
	node := testNode(t, multipartNode)
	for _, field := range []string{"a[0]", "a[b]", "a[][]", "a[]b", "[]"} {
		t.Run(field, func(t *testing.T) {
			_, err := buildParts(t, node, newFakeStore(), multipart.DefaultLimits(), []partSpec{
				{field: field, data: []byte("x")},
			})
			if err == nil {
				t.Fatalf("Build() error = nil, want field %q rejected", field)
			}
			if !errors.Is(err, model.ErrBadRequest) {
				t.Errorf("error = %v, want ErrBadRequest (400)", err)
			}
		})
	}
}

// A file past the node's max_file_bytes fails with 413 and commits no
// object; its key is still reported so the caller deletes any partial.
func TestBuildEnforcesNodeMaxFileBytes(t *testing.T) {
	node := testNode(t, `{"type":"input","channel":"http","accept":["multipart/form-data"],`+
		`"storage":{"bucket":"uploads","max_file_bytes":64}}`)
	store := newFakeStore()
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte("j"), 5000)...)
	res, err := buildParts(t, node, store, multipart.DefaultLimits(), []partSpec{
		{field: "images", filename: "big.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
	})
	if err == nil {
		t.Fatal("Build() error = nil, want the oversized file rejected")
	}
	if !errors.Is(err, model.ErrPayloadTooLarge) {
		t.Fatalf("error = %v, want ErrPayloadTooLarge (413)", err)
	}
	if store.objectCount() != 0 {
		t.Errorf("store holds %d objects, want no committed object", store.objectCount())
	}
	if res == nil || len(res.Keys) != 1 {
		t.Fatalf("keys = %v, want the failed key for best-effort cleanup", res)
	}
	// A file exactly at the cap passes: the abort fires past it, not on it.
	store = newFakeStore()
	exact := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte("j"), 60)...)
	res, err = buildParts(t, node, store, multipart.DefaultLimits(), []partSpec{
		{field: "images", filename: "exact.jpg", isFile: true, contentType: "image/jpeg", data: exact},
	})
	if err != nil {
		t.Fatalf("Build() error = %v, want the at-cap file accepted", err)
	}
	if len(res.Keys) != 1 || store.objectCount() != 1 {
		t.Errorf("keys = %v objects = %d, want the at-cap upload committed", res.Keys, store.objectCount())
	}
}

// Without a node cap the engine per-file cap applies.
func TestBuildEnforcesEngineMaxFileBytes(t *testing.T) {
	node := testNode(t, multipartNode)
	limits := multipart.DefaultLimits()
	limits.MaxFileBytes = 32
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte("j"), 100)...)
	_, err := buildParts(t, node, newFakeStore(), limits, []partSpec{
		{field: "images", filename: "a.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
	})
	if !errors.Is(err, model.ErrPayloadTooLarge) {
		t.Errorf("error = %v, want ErrPayloadTooLarge (413) from the engine cap", err)
	}
}

// The declared Content-Type alone never satisfies a specific allowlist
// entry: bytes sniffing as another type fail with 422 before any upload.
func TestBuildMIMEAllowlistUsesSniffedType(t *testing.T) {
	node := testNode(t, `{"type":"input","channel":"http","accept":["multipart/form-data"],`+
		`"storage":{"bucket":"uploads","allowed_mime_types":["image/png"]}}`)
	store := newFakeStore()
	// Declared image/png, but GIF bytes.
	gif := append([]byte("GIF89a"), bytes.Repeat([]byte("g"), 100)...)
	res, err := buildParts(t, node, store, multipart.DefaultLimits(), []partSpec{
		{field: "images", filename: "spoof.png", isFile: true, contentType: "image/png", data: gif},
	})
	if err == nil {
		t.Fatal("Build() error = nil, want the MIME-spoofed file rejected")
	}
	if !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid (422)", err)
	}
	if len(store.putCalls()) != 0 || store.objectCount() != 0 {
		t.Errorf("store saw %d puts, want the 422 before any upload", len(store.putCalls()))
	}
	if res == nil || len(res.Keys) != 0 {
		t.Errorf("keys = %v, want none (nothing uploaded)", res)
	}
}

// A type/* entry accepts any subtype of that type.
func TestBuildMIMEWildcardAcceptsSubtype(t *testing.T) {
	node := testNode(t, `{"type":"input","channel":"http","accept":["multipart/form-data"],`+
		`"storage":{"bucket":"uploads","allowed_mime_types":["image/*"]}}`)
	gif := append([]byte("GIF89a"), bytes.Repeat([]byte("g"), 100)...)
	res, err := buildParts(t, node, newFakeStore(), multipart.DefaultLimits(), []partSpec{
		{field: "images", filename: "a.gif", isFile: true, contentType: "application/octet-stream", data: gif},
	})
	if err != nil {
		t.Fatalf("Build() error = %v, want image/* to accept image/gif", err)
	}
	payload := decodePayload(t, res.Payload)
	images := asArray(t, payload, "images")
	if ref, _ := images[0].(map[string]any); ref["mime_type"] != "image/gif" {
		t.Errorf("mime_type = %v, want the sniffed image/gif", ref["mime_type"])
	}
}

// Bytes that sniff as application/octet-stream fall back to the declared
// Content-Type for the allowlist check.
func TestBuildMIMEFallsBackToDeclared(t *testing.T) {
	data := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}
	if got := http.DetectContentType(data); got != "application/octet-stream" {
		t.Fatalf("fixture sniffs as %q, want application/octet-stream", got)
	}
	node := testNode(t, `{"type":"input","channel":"http","accept":["multipart/form-data"],`+
		`"storage":{"bucket":"uploads","allowed_mime_types":["image/png"]}}`)
	res, err := buildParts(t, node, newFakeStore(), multipart.DefaultLimits(), []partSpec{
		{field: "images", filename: "a.bin", isFile: true, contentType: "image/png", data: data},
	})
	if err != nil {
		t.Fatalf("Build() error = %v, want the declared type to satisfy the allowlist", err)
	}
	payload := decodePayload(t, res.Payload)
	images := asArray(t, payload, "images")
	if ref, _ := images[0].(map[string]any); ref["mime_type"] != "image/png" {
		t.Errorf("mime_type = %v, want the declared image/png", ref["mime_type"])
	}
}

// Engine file-count, total-bytes, and text-part caps each fail with 413.
func TestBuildEnforcesEngineCaps(t *testing.T) {
	node := testNode(t, multipartNode)
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'j'}

	t.Run("max files", func(t *testing.T) {
		limits := multipart.DefaultLimits()
		limits.MaxFiles = 2
		_, err := buildParts(t, node, newFakeStore(), limits, []partSpec{
			{field: "a", filename: "a.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
			{field: "b", filename: "b.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
			{field: "c", filename: "c.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
		})
		if !errors.Is(err, model.ErrPayloadTooLarge) {
			t.Errorf("error = %v, want ErrPayloadTooLarge (413)", err)
		}
	})

	t.Run("max total", func(t *testing.T) {
		limits := multipart.DefaultLimits()
		limits.MaxTotalBytes = 9
		_, err := buildParts(t, node, newFakeStore(), limits, []partSpec{
			{field: "a", filename: "a.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
			{field: "b", filename: "b.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
		})
		if !errors.Is(err, model.ErrPayloadTooLarge) {
			t.Errorf("error = %v, want ErrPayloadTooLarge (413)", err)
		}
	})

	t.Run("max text", func(t *testing.T) {
		limits := multipart.DefaultLimits()
		limits.MaxTextBytes = 4
		_, err := buildParts(t, node, newFakeStore(), limits, []partSpec{
			{field: "name", data: []byte("way too long")},
		})
		if !errors.Is(err, model.ErrPayloadTooLarge) {
			t.Errorf("error = %v, want ErrPayloadTooLarge (413)", err)
		}
	})
}

// The extension is the lowercased suffix past the last dot, kept to
// [a-z0-9]; no dot means a bare UUID key.
func TestBuildSanitizesExtensions(t *testing.T) {
	node := testNode(t, `{"type":"input","channel":"http","accept":["multipart/form-data"],`+
		`"storage":{"bucket":"uploads"}}`)
	data := []byte("hello")
	cases := map[string]string{
		"photo.JPG":      ".jpg",
		"archive.tar.gz": ".gz",
		"noext":          "",
		"trailing.":      "",
		"weird.JP(E)G":   ".jpeg",
	}
	for filename, wantExt := range cases {
		t.Run(filename, func(t *testing.T) {
			res, err := buildParts(t, node, newFakeStore(), multipart.DefaultLimits(), []partSpec{
				{field: "f", filename: filename, isFile: true, contentType: "text/plain", data: data},
			})
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			payload := decodePayload(t, res.Payload)
			files := asArray(t, payload, "f")
			ref, _ := files[0].(map[string]any)
			fileName, _ := ref["file_name"].(string)
			if !strings.HasSuffix(fileName, wantExt) {
				t.Fatalf("file_name = %q, want suffix %q", fileName, wantExt)
			}
			stem := strings.TrimSuffix(fileName, wantExt)
			if !ids.Valid(stem) {
				t.Errorf("file_name stem = %q, want a uuid", stem)
			}
			if len(res.Keys) != 1 || res.Keys[0] != fileName {
				t.Errorf("keys = %v, want the bare file name (no prefix)", res.Keys)
			}
		})
	}
}

// The counted size spans the peek window: 1500 bytes read as 512 peeked
// plus the streamed remainder.
func TestBuildCountsSizeAcrossPeekWindow(t *testing.T) {
	node := testNode(t, multipartNode)
	data := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte("j"), 1496)...)
	if len(data) != 1500 {
		t.Fatalf("fixture is %d bytes, want 1500", len(data))
	}
	store := newFakeStore()
	res, err := buildParts(t, node, store, multipart.DefaultLimits(), []partSpec{
		{field: "f", filename: "a.jpg", isFile: true, contentType: "image/jpeg", data: data},
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	payload := decodePayload(t, res.Payload)
	files := asArray(t, payload, "f")
	if ref, _ := files[0].(map[string]any); ref["size"] != float64(1500) {
		t.Errorf("size = %v, want 1500 counted bytes", ref["size"])
	}
	obj, ok := store.object("uploads", res.Keys[0])
	if !ok {
		t.Fatalf("bucket holds no object for key %q", res.Keys[0])
	}
	if !bytes.Equal(obj.data, data) {
		t.Error("stored bytes differ from the uploaded part")
	}
}

// Without node storage the builder fails closed instead of uploading to an
// unknown bucket.
func TestBuildRequiresNodeStorage(t *testing.T) {
	node := testNode(t, `{"type":"input","channel":"http"}`)
	body, ct := buildBody(t, []partSpec{{field: "name", data: []byte("x")}})
	res, err := multipart.Build(context.Background(), testReader(t, body, ct), node, newFakeStore(), multipart.DefaultLimits())
	if err == nil {
		t.Fatal("Build() error = nil, want failure without node storage")
	}
	if res == nil {
		t.Fatal("result is nil, want a non-nil result even on failure")
	}
}

type failingBody struct{ err error }

func (f *failingBody) Read([]byte) (int, error) { return 0, f.err }

func TestBuildPreservesBodyReadCauseChain(t *testing.T) {
	cause := fmt.Errorf("boom: %w", io.ErrUnexpectedEOF)
	mr := stdmultipart.NewReader(&failingBody{err: cause}, "boundary")
	node := testNode(t, multipartNode)
	_, err := multipart.Build(context.Background(), mr, node, newFakeStore(), multipart.DefaultLimits())
	if err == nil {
		t.Fatal("Build() error = nil, want body read failure")
	}
	if !errors.Is(err, model.ErrBadRequest) {
		t.Errorf("errors.Is(err, ErrBadRequest) = false, want true (%v)", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("errors.Is(err, cause) = false, want cause chain preserved (%v)", err)
	}
}

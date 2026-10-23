package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	stdmultipart "mime/multipart"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/auth"
	"github.com/beelinewf/wf-engine/internal/workflow/executor"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/multipart"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"github.com/beelinewf/wf-engine/internal/workflow/service"
	"gorm.io/gorm"
)

// mpFakeStore is an in-memory object store: uploads commit bytes, deletes
// remove them, and every Put attempt is counted so tests prove replays
// upload nothing.
type mpFakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
	deletes []string
}

func newMpFakeStore() *mpFakeStore {
	return &mpFakeStore{objects: map[string][]byte{}}
}

func (f *mpFakeStore) Put(_ context.Context, bucket, key string, r io.Reader, _ int64, _ string, _ map[string]string) error {
	data, err := io.ReadAll(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if err != nil {
		return err
	}
	f.objects[bucket+"/"+key] = data
	return nil
}

func (f *mpFakeStore) Delete(_ context.Context, bucket, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, bucket+"/"+key)
	delete(f.objects, bucket+"/"+key)
	return nil
}

func (f *mpFakeStore) stats() (puts, objects int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts, len(f.objects)
}

// svcMultipartService mirrors production wiring plus the fake object store
// multipart deliveries upload through.
func svcMultipartService(db *gorm.DB, store *mpFakeStore) service.InstanceService {
	instances := repository.NewInstanceRepositoryWithOptions(db, model.LeanOptions{})
	return service.NewInstanceServiceWithMultipart(
		instances,
		repository.NewParallelRepository(db),
		repository.NewWorkflowDefinitionRepository(db),
		repository.NewSecretRepository(db),
		repository.NewFormRepository(db),
		svcWorkflowService(db),
		&executor.InputExecutor{},
		executor.NewHookRunner(nil),
		svcSysUserID,
		svcLimits,
		nil,
		model.LeanOptions{},
		auth.Catalog{},
		service.MultipartDeps{Store: store, Limits: multipart.DefaultLimits()},
	)
}

// mpPart is one multipart part of a test delivery.
type mpPart struct {
	field       string
	filename    string
	isFile      bool
	contentType string
	data        []byte
}

func mpBody(t *testing.T, parts []mpPart) (body []byte, contentType string) {
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
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, p.field, p.filename))
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

// mpDelivery builds the DeliverInput request for a streaming body. The
// factory opens a fresh reader per call, so replays reuse the helper.
func mpDelivery(t *testing.T, instanceID, key string, parts []mpPart) service.DeliverInput {
	t.Helper()
	body, ct := mpBody(t, parts)
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("parse content type: %v", err)
	}
	return service.DeliverInput{
		InstanceID:     instanceID,
		IdempotencyKey: key,
		ContentType:    model.MediaTypeMultipart,
		MultipartSource: func() (*stdmultipart.Reader, error) {
			return stdmultipart.NewReader(bytes.NewReader(body), params["boundary"]), nil
		},
	}
}

func mpInputExtra() map[string]any {
	return map[string]any{
		"channel": "http", "output_property": "submission",
		"accept": []string{"application/json", "multipart/form-data"},
		"storage": map[string]any{
			"bucket": "uploads", "prefix": "submissions/",
			"allowed_mime_types": []string{"image/*"},
		},
	}
}

func mpImageParts() []mpPart {
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte("j"), 100)...)
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte("p"), 200)...)
	return []mpPart{
		{field: "name", data: []byte("Ada")},
		{field: "images[]", filename: "a.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
		{field: "images[]", filename: "b.png", isFile: true, contentType: "image/png", data: png},
		{field: "images[]", filename: "c.jpg", isFile: true, contentType: "image/jpeg", data: jpeg},
	}
}

func mpParkedInstance(t *testing.T, db *gorm.DB, svc service.InstanceService, extra map[string]any) string {
	t.Helper()
	ctx := context.Background()
	n1 := "11111111-1111-7111-8111-111111111101"
	wfID := svcCreateWorkflow(t, db, n1,
		svcNodeJSON(n1, "input", "ask", "", "", extra),
	)
	inst, err := svc.Create(ctx, service.CreateInstance{WorkflowDefinitionID: wfID, Context: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	cur := driveEngine(t, db, inst.ID)
	if cur.Status != model.WorkflowWaiting || cur.WaitingReason != model.WaitingReasonInput {
		t.Fatalf("instance = %+v, want waiting on input", cur)
	}
	return inst.ID
}

func mpContext(t *testing.T, svc service.InstanceService, instanceID string) map[string]any {
	t.Helper()
	got, err := svc.GetContext(context.Background(), instanceID, auth.Principal{})
	if err != nil {
		t.Fatalf("GetContext() error = %v", err)
	}
	var ctxMap map[string]any
	if err := json.Unmarshal(got.Context, &ctxMap); err != nil {
		t.Fatalf("parse context: %v", err)
	}
	return ctxMap
}

// A multipart delivery resumes the instance with the text field as a
// one-element string array and the file fields as enriched references under
// output_property, while the bucket holds the uploaded objects.
func TestDeliverMultipartResumesWithReferences(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	store := newMpFakeStore()
	svc := svcMultipartService(db, store)
	instID := mpParkedInstance(t, db, svc, mpInputExtra())

	delivery, err := svc.DeliverInput(ctx, mpDelivery(t, instID, "mp-1", mpImageParts()))
	if err != nil {
		t.Fatalf("DeliverInput() error = %v", err)
	}
	if !delivery.Accepted {
		t.Fatalf("delivery = %+v, want accepted", delivery)
	}
	submission, ok := mpContext(t, svc, instID)["submission"].(map[string]any)
	if !ok {
		t.Fatalf("context has no submission object: %v", mpContext(t, svc, instID))
	}
	name, ok := submission["name"].([]any)
	if !ok || len(name) != 1 || name[0] != "Ada" {
		t.Errorf("name = %v, want [Ada]", submission["name"])
	}
	images, ok := submission["images"].([]any)
	if !ok || len(images) != 3 {
		t.Fatalf("images = %v, want 3 file references", submission["images"])
	}
	for i, e := range images {
		ref, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("images[%d] = %T, want a reference object", i, e)
		}
		for _, k := range []string{"original_file_name", "file_name", "size", "mime_type"} {
			if _, ok := ref[k]; !ok {
				t.Errorf("images[%d] has no %q (%v)", i, k, ref)
			}
		}
	}
	if images[0].(map[string]any)["original_file_name"] != "a.jpg" ||
		images[1].(map[string]any)["original_file_name"] != "b.png" {
		t.Errorf("original names out of part order: %v", images)
	}
	if images[0].(map[string]any)["size"] != float64(104) ||
		images[1].(map[string]any)["size"] != float64(208) {
		t.Errorf("sizes = %v/%v, want counted bytes 104/208",
			images[0].(map[string]any)["size"], images[1].(map[string]any)["size"])
	}
	if puts, objects := store.stats(); puts != 3 || objects != 3 {
		t.Errorf("store puts = %d objects = %d, want 3 committed uploads", puts, objects)
	}
	store.mu.Lock()
	for k := range store.objects {
		if len(k) < len("uploads/submissions/") || k[:len("uploads/submissions/")] != "uploads/submissions/" {
			t.Errorf("object %q is not under uploads/submissions/", k)
		}
	}
	store.mu.Unlock()
}

// The validation script sees size/mime_type/array length and can reject on
// them; the rejection persists Accepted=false and deletes the uploads.
func TestDeliverMultipartScriptRejectsAndCleansUp(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	store := newMpFakeStore()
	svc := svcMultipartService(db, store)
	extra := mpInputExtra()
	extra["validation"] = map[string]any{
		"script": `input = JSON.parse(input);
if (input.images.length > 2) { return 'at most two images'; }
if (input.images[0].size > 150) { return 'first image too big'; }
if (input.images[0].mime_type !== 'image/jpeg') { return 'first image must be jpeg'; };`,
	}
	instID := mpParkedInstance(t, db, svc, extra)

	delivery, err := svc.DeliverInput(ctx, mpDelivery(t, instID, "mp-reject", mpImageParts()))
	if err != nil {
		t.Fatalf("DeliverInput() error = %v", err)
	}
	if delivery.Accepted {
		t.Fatalf("delivery = %+v, want rejected by the script", delivery)
	}
	if delivery.Error != "at most two images" {
		t.Errorf("error = %q, want the script message", delivery.Error)
	}
	if puts, objects := store.stats(); puts != 3 || objects != 0 {
		t.Errorf("store puts = %d objects = %d, want 3 uploads all cleaned up", puts, objects)
	}
	if len(store.deletes) != 3 {
		t.Errorf("deletes = %d, want best-effort deletion of the 3 uploads", len(store.deletes))
	}
}

// A multipart replay returns the stored delivery without re-uploading and
// without the JSON path's payload-equality 409: even a different body replays.
func TestDeliverMultipartReplaySkipsReupload(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	store := newMpFakeStore()
	svc := svcMultipartService(db, store)
	instID := mpParkedInstance(t, db, svc, mpInputExtra())

	first, err := svc.DeliverInput(ctx, mpDelivery(t, instID, "mp-replay", mpImageParts()))
	if err != nil {
		t.Fatalf("DeliverInput() error = %v", err)
	}
	putsBefore, _ := store.stats()
	other := []mpPart{{field: "name", data: []byte("changed entirely")}}
	replay, err := svc.DeliverInput(ctx, mpDelivery(t, instID, "mp-replay", other))
	if err != nil {
		t.Fatalf("replay DeliverInput() error = %v", err)
	}
	if replay.ID != first.ID {
		t.Fatalf("replay.ID = %s, want %s", replay.ID, first.ID)
	}
	if putsAfter, _ := store.stats(); putsAfter != putsBefore {
		t.Errorf("store puts grew %d -> %d on replay, want no re-upload", putsBefore, putsAfter)
	}
}

// Without infra.storage a multipart delivery fails at delivery time with a
// clear error; definitions still parse.
func TestDeliverMultipartStorageDisabled(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	svc := svcInstanceService(db)
	instID := mpParkedInstance(t, db, svc, mpInputExtra())

	_, err := svc.DeliverInput(ctx, mpDelivery(t, instID, "mp-nostore", mpImageParts()))
	if err == nil {
		t.Fatal("DeliverInput() error = nil, want storage-disabled failure")
	}
	// A plain error maps to 500, which is what an unconfigured deployment
	// must answer: no client sentinel may wrap it.
	for _, sentinel := range []error{
		model.ErrNotFound, model.ErrInvalid, model.ErrConflict, model.ErrForbidden,
		model.ErrBadRequest, model.ErrPayloadTooLarge, model.ErrUnsupportedMedia,
		model.ErrTerminalState,
	} {
		if errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want a plain 500-mapped error, not %v", err, sentinel)
		}
	}
	if !strings.Contains(err.Error(), "infra.storage") {
		t.Errorf("error = %q, want it to name infra.storage", err)
	}
}

// A delivery whose media type the node does not list is refused with 415,
// in both directions.
func TestDeliverInputRejectsUnacceptedMediaType(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	store := newMpFakeStore()
	svc := svcMultipartService(db, store)

	t.Run("multipart to json-only node", func(t *testing.T) {
		instID := mpParkedInstance(t, db, svc, map[string]any{"channel": "http", "output_property": "submission"})
		_, err := svc.DeliverInput(ctx, mpDelivery(t, instID, "mp-415", mpImageParts()))
		if !errors.Is(err, model.ErrUnsupportedMedia) {
			t.Errorf("error = %v, want ErrUnsupportedMedia (415)", err)
		}
		if puts, _ := store.stats(); puts != 0 {
			t.Errorf("store puts = %d, want no upload before the accept check", puts)
		}
	})

	t.Run("json to multipart-only node", func(t *testing.T) {
		extra := map[string]any{
			"channel": "http", "output_property": "submission",
			"accept":  []string{"multipart/form-data"},
			"storage": map[string]any{"bucket": "uploads"},
		}
		instID := mpParkedInstance(t, db, svc, extra)
		_, err := svc.DeliverInput(ctx, service.DeliverInput{
			InstanceID: instID, IdempotencyKey: "json-415", Payload: []byte(`{"a":1}`),
		})
		if !errors.Is(err, model.ErrUnsupportedMedia) {
			t.Errorf("error = %v, want ErrUnsupportedMedia (415)", err)
		}
	})
}

// A branch-parked multipart delivery advances only the branch, with the same
// reference shape and the same cleanup guarantee.
func TestDeliverMultipartToBranchParkedInput(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	store := newMpFakeStore()
	svc := svcMultipartService(db, store)
	wfID := svcCreateBranchInputWorkflow(t, db, mpInputExtra())
	instID, a, _ := svcSetupBranchInput(t, db, svc, wfID)

	req := mpDelivery(t, instID, "mp-branch", mpImageParts())
	req.BranchID = a.ID
	delivery, err := svc.DeliverInput(ctx, req)
	if err != nil {
		t.Fatalf("DeliverInput() error = %v", err)
	}
	if !delivery.Accepted {
		t.Fatalf("delivery = %+v, want accepted", delivery)
	}
	branch, err := repository.NewParallelRepository(db).GetBranch(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetBranch() error = %v", err)
	}
	var branchCtx map[string]any
	if err := json.Unmarshal(branch.Context, &branchCtx); err != nil {
		t.Fatalf("parse branch context: %v", err)
	}
	submission, ok := branchCtx["submission"].(map[string]any)
	if !ok {
		t.Fatalf("branch context has no submission object: %v", branchCtx)
	}
	images, ok := submission["images"].([]any)
	if !ok || len(images) != 3 {
		t.Fatalf("images = %v, want 3 file references", submission["images"])
	}
	if puts, objects := store.stats(); puts != 3 || objects != 3 {
		t.Errorf("store puts = %d objects = %d, want 3 committed uploads", puts, objects)
	}
}

// A node referencing a node definition inherits accept/storage from the
// definition: the occurrence carries neither, yet the multipart delivery is
// accepted with the definition's bucket and constraints.
func TestDeliverMultipartToReferencedNode(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	store := newMpFakeStore()
	svc := svcMultipartService(db, store)

	ndID := "11111111-1111-7111-8111-111111111301"
	nodeDef := model.NodeDefinition{
		ID: ndID, Name: "ask", Version: 1, LineageID: svcNewID(),
		Type: "input", Content: json.RawMessage(`{"type":"input","channel":"http",` +
			`"output_property":"submission","accept":["multipart/form-data"],` +
			`"storage":{"bucket":"uploads","prefix":"ref/"}}`),
		CreatedBy: svcSysUserID, UpdatedBy: svcSysUserID,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := repository.NewNodeDefinitionRepository(db).Create(ctx, nodeDef); err != nil {
		t.Fatalf("create node definition: %v", err)
	}
	wfID := svcCreateWorkflow(t, db, "11111111-1111-7111-8111-111111111302",
		`{"id":"11111111-1111-7111-8111-111111111302","name":"ask","node_definition_id":"`+ndID+`"}`,
	)
	inst, err := svc.Create(ctx, service.CreateInstance{WorkflowDefinitionID: wfID, Context: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	cur := driveEngine(t, db, inst.ID)
	if cur.Status != model.WorkflowWaiting || cur.WaitingReason != model.WaitingReasonInput {
		t.Fatalf("instance = %+v, want waiting on input", cur)
	}
	delivery, err := svc.DeliverInput(ctx, mpDelivery(t, inst.ID, "mp-ref", mpImageParts()))
	if err != nil {
		t.Fatalf("DeliverInput() error = %v", err)
	}
	if !delivery.Accepted {
		t.Fatalf("delivery = %+v, want accepted via the inherited contract", delivery)
	}
	submission, ok := mpContext(t, svc, inst.ID)["submission"].(map[string]any)
	if !ok {
		t.Fatalf("context has no submission object: %v", mpContext(t, svc, inst.ID))
	}
	if images, ok := submission["images"].([]any); !ok || len(images) != 3 {
		t.Fatalf("images = %v, want 3 file references", submission["images"])
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.objects) != 3 {
		t.Fatalf("store holds %d objects, want 3", len(store.objects))
	}
	for k := range store.objects {
		if len(k) < len("uploads/ref/") || k[:len("uploads/ref/")] != "uploads/ref/" {
			t.Errorf("object %q is not under the definition's uploads/ref/ prefix", k)
		}
	}
}

// Status reports the effective accept list: multipart where the node opts
// in, JSON only where it predates the field.
func TestPendingInputReportsAccept(t *testing.T) {
	db := setupSvcDB(t)
	ctx := context.Background()
	svc := svcInstanceService(db)

	mpID := mpParkedInstance(t, db, svc, mpInputExtra())
	detail, err := svc.GetStatusDetail(ctx, mpID, auth.Principal{})
	if err != nil {
		t.Fatalf("GetStatusDetail() error = %v", err)
	}
	if detail.PendingInput == nil {
		t.Fatal("PendingInput is nil, want the waiting-input contract")
	}
	if len(detail.PendingInput.Accept) != 2 {
		t.Errorf("accept = %v, want both media types", detail.PendingInput.Accept)
	}

	legacyID := mpParkedInstance(t, db, svc, map[string]any{"channel": "http", "output_property": "submission"})
	legacy, err := svc.GetStatusDetail(ctx, legacyID, auth.Principal{})
	if err != nil {
		t.Fatalf("GetStatusDetail() error = %v", err)
	}
	if got := legacy.PendingInput.Accept; len(got) != 1 || got[0] != model.MediaTypeJSON {
		t.Errorf("accept = %v, want [application/json] for a legacy node", got)
	}
}
